//go:build windows

package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"time"

	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"
)

const serviceName = "IDLifecycle"

func runService(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("service", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgPath := configFlag(fs)
	if err := fs.Parse(args); err != nil {
		return 2
	}

	isSvc, err := svc.IsWindowsService()
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	if !isSvc {
		fmt.Fprintln(stderr, "the 'service' command is for the Service Control Manager; use 'run' from a console")
		return 2
	}

	elog, _ := eventlog.Open(serviceName) // best effort: needs the event source registered at install time
	h := &handler{cfg: *cfgPath, elog: elog}
	if err := svc.Run(serviceName, h); err != nil {
		if elog != nil {
			_ = elog.Error(1, "service failed: "+err.Error())
		}
		return 1
	}
	return 0
}

type handler struct {
	cfg  string
	elog *eventlog.Log
}

func (h *handler) Execute(_ []string, requests <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	const accepted = svc.AcceptStop | svc.AcceptShutdown
	status <- svc.Status{State: svc.StartPending, WaitHint: 30000}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- serve(ctx, h.cfg, false) }()

	// Report Running once the process is up; a failed start surfaces through done below.
	select {
	case err := <-done:
		if h.elog != nil {
			_ = h.elog.Error(2, "IDLifecycle failed to start: "+errString(err))
		}
		return true, 1
	case <-time.After(3 * time.Second):
	}
	status <- svc.Status{State: svc.Running, Accepts: accepted}
	if h.elog != nil {
		_ = h.elog.Info(1, "IDLifecycle started")
	}

	for {
		select {
		case err := <-done:
			if h.elog != nil {
				_ = h.elog.Error(3, "IDLifecycle stopped unexpectedly: "+errString(err))
			}
			return true, 1
		case c := <-requests:
			switch c.Cmd {
			case svc.Interrogate:
				status <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				status <- svc.Status{State: svc.StopPending, WaitHint: 60000}
				cancel()
				select {
				case <-done:
				case <-time.After(55 * time.Second):
				}
				if h.elog != nil {
					_ = h.elog.Info(4, "IDLifecycle stopped")
				}
				return false, 0
			}
		}
	}
}

func errString(err error) string {
	if err == nil {
		return "exited"
	}
	return err.Error()
}
