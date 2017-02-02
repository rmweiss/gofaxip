// This file is part of the GOfax.IP project - https://github.com/gonicus/gofaxip
// Copyright (C) 2014 GONICUS GmbH, Germany - http://www.gonicus.de
//
// This program is free software; you can redistribute it and/or
// modify it under the terms of the GNU General Public License
// as published by the Free Software Foundation; version 2
// of the License.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with this program; if not, write to the Free Software
// Foundation, Inc., 51 Franklin Street, Fifth Floor, Boston, MA  02110-1301, USA.

package main

import (
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/gonicus/gofaxip/gofaxlib"

	"github.com/fiorix/go-eventsocket/eventsocket"
)

type transmission struct {
	faxjob FaxJob
	conn   *eventsocket.Connection

	pageChan   chan *gofaxlib.PageResult
	errorChan  chan FaxError
	resultChan chan *gofaxlib.FaxResult

	sessionlog gofaxlib.SessionLogger
}

func transmit(faxjob FaxJob, sessionlog gofaxlib.SessionLogger) *transmission {
	t := &transmission{
		faxjob:     faxjob,
		pageChan:   make(chan *gofaxlib.PageResult),
		errorChan:  make(chan FaxError),
		resultChan: make(chan *gofaxlib.FaxResult),
		sessionlog: sessionlog,
	}
	go t.start()
	return t
}

func (t *transmission) PageSent() <-chan *gofaxlib.PageResult {
	return t.pageChan
}

func (t *transmission) Errors() <-chan FaxError {
	return t.errorChan
}

func (t *transmission) Result() <-chan *gofaxlib.FaxResult {
	return t.resultChan
}

// Connect to FreeSWITCH and originate a txfax
func (t *transmission) start() {

	if t.faxjob.Number == "" {
		t.errorChan <- NewFaxError("Number to dial is empty", false)
		return
	}

	if len(t.faxjob.Gateways) == 0 {
		t.errorChan <- NewFaxError("Gateway not set", false)
		return
	}

	if _, err := os.Stat(t.faxjob.Filename); err != nil {
		t.errorChan <- NewFaxError(err.Error(), false)
		return
	}

	var err error
	t.conn, err = eventsocket.Dial(gofaxlib.Config.Freeswitch.Socket, gofaxlib.Config.Freeswitch.Password)
	if err != nil {
		t.errorChan <- NewFaxError(err.Error(), true)
		return
	}
	defer t.conn.Close()

	// Enable event filter and events
	_, err = t.conn.Send(fmt.Sprintf("filter Unique-ID %v", t.faxjob.UUID))
	if err != nil {
		t.errorChan <- NewFaxError(err.Error(), true)
		return
	}
	_, err = t.conn.Send("event plain CHANNEL_CALLSTATE CUSTOM spandsp::txfaxnegociateresult spandsp::txfaxpageresult spandsp::txfaxresult")
	if err != nil {
		t.errorChan <- NewFaxError(err.Error(), true)
		return
	}

	// Check if T.38 should be disabled
	disableT38 := gofaxlib.Config.Freeswitch.DisableT38
	if disableT38 {
		t.sessionlog.Log("T.38 disabled by configuration")
	} else {
		disableT38, err = gofaxlib.GetSoftmodemFallback(t.conn, t.faxjob.Number)
		if err != nil {
			t.sessionlog.Log(err)
			disableT38 = false
		}
		if disableT38 {
			t.sessionlog.Log(fmt.Sprintf("Softmodem fallback active for destination %s, disabling T.38", t.faxjob.Number))
		}
	}

	// Assemble dialstring
	dsVariablesMap := map[string]string{
		"ignore_early_media":           "true",
		"origination_uuid":             t.faxjob.UUID.String(),
		// "origination_caller_id_number": t.faxjob.Cidnum,
		// "origination_caller_id_name":   t.faxjob.Cidname,
		"sip_from_uri":                 fmt.Sprintf("sip:%v@${local_ip_v4}", t.faxjob.Cidnum),
		"fax_ident":                    t.faxjob.Ident,
		"fax_header":                   t.faxjob.Header,
		"fax_use_ecm":                  strconv.FormatBool(t.faxjob.UseECM),
		"fax_disable_v17":              strconv.FormatBool(t.faxjob.DisableV17),
		"fax_verbose":                  strconv.FormatBool(gofaxlib.Config.Freeswitch.Verbose),
	}

	if disableT38 {
		dsVariablesMap["fax_enable_t38"] = "false"
	} else {
		dsVariablesMap["fax_enable_t38"] = "true"
	}

	dsVariablesPairs := make([]string, len(dsVariablesMap))
	i := 0
	for k, v := range dsVariablesMap {
		dsVariablesPairs[i] = fmt.Sprintf("%v='%v'", k, v)
		i++
	}
	dsVariables := strings.Join(dsVariablesPairs, ",")

	translate := gofaxlib.Config.Freeswitch.Translate
	translateTo := gofaxlib.Config.Freeswitch.TranslateTo

	// Try gateways in configured order
	dsGatewaysStrings := make([]string, len(t.faxjob.Gateways))
	for i, gw := range t.faxjob.Gateways {
		if translate {
			dsGatewaysStrings[i] = fmt.Sprintf("sofia/gateway/%v/${translate(%v %v)}", gw, t.faxjob.Number, translateTo)
		} else {
			dsGatewaysStrings[i] = fmt.Sprintf("sofia/gateway/%v/%v", gw, t.faxjob.Number)
		}
	}
	dsGateways := strings.Join(dsGatewaysStrings, "|")

	dialstring := fmt.Sprintf("{%v}%v", dsVariables, dsGateways)
	//t.sessionlog.Log(fmt.Sprintf("%v Dialstring: %v", faxjob.UUID, dialstring))

	// Originate call
	t.sessionlog.Log("Originating channel to", t.faxjob.Number, "using gateway", strings.Join(t.faxjob.Gateways, ","))
	_, err = t.conn.Send(fmt.Sprintf("api expand originate %v, &txfax(%v)", dialstring, t.faxjob.Filename))
	if err != nil {
		t.conn.Send(fmt.Sprintf("uuid_dump %v", t.faxjob.UUID))
		hangupcause := strings.TrimSpace(err.Error())
		t.sessionlog.Log("Originate failed with hangup cause", hangupcause)
		t.errorChan <- NewFaxError(hangupcause, true)
		return
	}
	t.sessionlog.Log("Originate successful")

	result := gofaxlib.NewFaxResult(t.faxjob.UUID, t.sessionlog)

	es := gofaxlib.NewEventStream(t.conn)
	var pages uint

	// Listen for system signals to be able to kill the channel
	sigchan := make(chan os.Signal, 1)
	signal.Notify(sigchan, syscall.SIGTERM, syscall.SIGINT)

	for {
		select {
		case ev := <-es.Events():
			result.AddEvent(ev)
			if result.Hangupcause != "" {

				// If transmission failed:
				// Check if softmodem fallback should be enabled on the next call
				if gofaxlib.Config.Freeswitch.SoftmodemFallback && !result.Success {
					var activateFallback bool

					if result.NegotiateCount > 1 {
						// Activate fallback if negotiation was repeated
						t.sessionlog.Log(fmt.Sprintf("Fax failed with %d negotiations, enabling softmodem fallback for calls from/to %s.", result.NegotiateCount, t.faxjob.Number))
						activateFallback = true
					} else {
						var badrows uint
						for _, p := range result.PageResults {
							badrows += p.BadRows
						}
						if badrows > 0 {
							// Activate fallback if any bad rows were present
							t.sessionlog.Log(fmt.Sprintf("Fax failed with %d bad rows in %d pages, enabling softmodem fallback for calls from/to %s.", badrows, result.TransferredPages, t.faxjob.Number))
							activateFallback = true
						}
					}

					if activateFallback {
						err = gofaxlib.SetSoftmodemFallback(t.conn, t.faxjob.Number, true)
						if err != nil {
							t.sessionlog.Log(err)
						}
					}

				}

				t.resultChan <- result
				return
			}
			if ev.Get("Event-Subclass") == "spandsp::txfaxnegociateresult" {
				t.resultChan <- result
			} else if result.TransferredPages != pages {
				pages = result.TransferredPages
				t.pageChan <- &result.PageResults[pages-1]
			}
		case err := <-es.Errors():
			t.errorChan <- NewFaxError(err.Error(), true)
			return
		case kill := <-sigchan:
			t.sessionlog.Log(fmt.Sprintf("%v Received signal %v, destroying channel", t.faxjob.UUID, kill))
			t.conn.Send(fmt.Sprintf("api uuid_kill %v", t.faxjob.UUID))
			os.Remove(t.faxjob.Filename)
			t.errorChan <- NewFaxError(fmt.Sprintf("Killed by signal %v", kill), false)
		}
	}

}
