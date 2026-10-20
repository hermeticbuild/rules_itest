package process_state_race_test

import (
	"context"
	"os"
	"runtime"
	"testing"
	"time"

	"rules_itest/runner"
)

func TestProcessStateAccessIsSynchronized(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	instance := &runner.ServiceInstance{}
	instance.Type = "service"
	instance.Label = "process-state-race-helper"
	instance.Exe = exe
	instance.Args = []string{"-test.run=^TestProcessStateRaceHelper$"}
	instance.Env = map[string]string{"GO_WANT_PROCESS_STATE_RACE_HELPER": "1"}

	if err := instance.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	readerStarted := make(chan struct{})
	stopReader := make(chan struct{})
	readerDone := make(chan struct{})
	go func() {
		close(readerStarted)
		defer close(readerDone)
		for {
			select {
			case <-stopReader:
				return
			default:
				_ = instance.ProcessState()
				runtime.Gosched()
			}
		}
	}()

	<-readerStarted
	if err := instance.Wait(); err != nil {
		t.Fatal(err)
	}
	close(stopReader)
	<-readerDone

	if state := instance.ProcessState(); state == nil {
		t.Fatal("process state is nil after Wait")
	}
}

func TestProcessStateRaceHelper(t *testing.T) {
	if os.Getenv("GO_WANT_PROCESS_STATE_RACE_HELPER") != "1" {
		return
	}
	time.Sleep(10 * time.Millisecond)
}
