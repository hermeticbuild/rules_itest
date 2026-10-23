package main

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"
)

func TestStartRun(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		previousRun := time.Now()
		time.Sleep(2 * time.Second)
		runStart := startRun(func() { time.Sleep(time.Second) })
		if got := runStart.Sub(previousRun); got != 2*time.Second {
			t.Fatalf("run started %s after previous run, want 2s", got)
		}
		if got := time.Since(runStart); got != time.Second {
			t.Fatalf("run elapsed time = %s, want 1s of service startup", got)
		}
	})
}

func TestPersistentRunDoesNotRepeatTest(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	tests := make(chan error, 1)
	tests <- nil
	reports := 0
	event := waitForRun(ctx, false, nil, tests, nil, nil, nil, func() {
		reports++
		cancel()
	})
	if event != runShutdown || reports != 1 {
		t.Fatalf("got event=%d, reports=%d; want shutdown and one test report", event, reports)
	}
}

func TestSuccessfulReloadReportsCompletedTest(t *testing.T) {
	builds := make(chan string, 1)
	builds <- successfulBuild
	tests := make(chan error, 1)
	reports := 0
	event := waitForRun(t.Context(), false, builds, tests, nil, func() {
		t.Fatal("canceled an already-completed test")
	}, func([]byte) (func(), error) {
		tests <- errors.New("test failed")
		return func() {
			if reports != 1 {
				t.Fatalf("got %d reports before restarting, want 1", reports)
			}
		}, nil
	}, func() { reports++ })
	if event != runComplete || reports != 1 {
		t.Fatalf("got event=%d, reports=%d; want completed reload and one test report", event, reports)
	}
}

func TestSuccessfulReloadWaitsForRunningTest(t *testing.T) {
	for _, tc := range []struct {
		name string
		want runEvent
	}{
		{"restart", runComplete},
		{"shutdown", runShutdown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				builds := make(chan string, 1)
				builds <- successfulBuild
				tests := make(chan error)
				canceled := make(chan struct{})
				done := make(chan runEvent, 1)
				restarted, reports := false, 0
				go func() {
					done <- waitForRun(ctx, false, builds, tests, nil, func() {
						close(canceled)
					}, func([]byte) (func(), error) {
						return func() { restarted = true }, nil
					}, func() { reports++ })
				}()
				<-canceled
				synctest.Wait()
				if restarted {
					t.Fatal("restarted before the test exited")
				}
				if tc.want == runShutdown {
					cancel()
				}
				tests <- errors.New("test exited")
				if event := <-done; event != tc.want || reports != 1 || restarted != (tc.want == runComplete) {
					t.Fatalf("got event=%d, reports=%d, restarted=%t; want event=%d, one report, restarted=%t", event, reports, restarted, tc.want, tc.want == runComplete)
				}
			})
		})
	}
}

func TestWaitForSuccessfulReload(t *testing.T) {
	missingSpecs := filepath.Join(t.TempDir(), "missing.json")
	synctest.Test(t, func(t *testing.T) {
		builds := make(chan string)
		tests, services := make(chan error), make(chan error)
		go func() {
			builds <- "IBAZEL_BUILD_STARTED"
			builds <- successfulBuild
			services <- errors.New("service crashed")
			tests <- nil
			builds <- "IBAZEL_BUILD_COMPLETED FAILURE"
			builds <- successfulBuild
		}()
		attempts, reports := 0, 0
		event := waitForRun(t.Context(), false, builds, tests, services, func() {
			t.Fatal("canceled the test for a rejected reload")
		}, func(notification []byte) (func(), error) {
			if got := string(notification); got != "IBAZEL_BUILD_COMPLETED SUCCESS\n" {
				t.Fatalf("forwarded notification %q", got)
			}
			attempts++
			if attempts == 1 {
				_, err := readServiceSpecs(missingSpecs)
				return nil, err
			}
			return func() {}, nil
		}, func() { reports++ })
		if event != runComplete || attempts != 2 || reports != 1 {
			t.Fatalf("got event=%d, reloads=%d, reports=%d; want completed run, 2 reloads, 1 report", event, attempts, reports)
		}
	})
}

func TestOneShotDoesNotIgnoreServiceFailure(t *testing.T) {
	tests, services := make(chan error, 1), make(chan error, 1)
	tests <- nil
	services <- errors.New("service crashed")
	event := waitForRun(t.Context(), true, nil, tests, services, nil, nil, func() {
		t.Fatal("reported a successful test after a service failure")
	})
	if event != runServiceFailed {
		t.Fatalf("got event %d, want service failure", event)
	}
}
