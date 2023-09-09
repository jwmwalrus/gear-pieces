package idler

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"slices"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	DefaultServerIdleTimeout = 300
)

var (
	// ServerIdleTimeout Amount of idle seconds before server exits.
	ServerIdleTimeout = DefaultServerIdleTimeout
)

// Status defines the idle status type.
type Status int

const (
	// StatusIdle Server is idle.
	StatusIdle Status = iota

	// StatusEngineLoop The engine loop is working.
	StatusEngineLoop

	// StatusRequest A client's request is being processed.
	StatusRequest

	// StatusSubscription A client subscription is active.
	StatusSubscription

	// StatusDbOperations A DB-related operation is in progress.
	StatusDbOperations

	// StatusFileOperations A file-related operation is in progress.
	StatusFileOperations
)

var (
	statusMap = map[Status]string{
		StatusIdle:           "idle",
		StatusEngineLoop:     "engine-loop",
		StatusRequest:        "request",
		StatusSubscription:   "subscription",
		StatusDbOperations:   "db-operations",
		StatusFileOperations: "file-operations",
	}
)

func (is Status) String() string {
	s, ok := statusMap[is]
	if ok {
		return s
	}

	return ""
}

var (
	idleStarted bool
	idleCancel  context.CancelFunc
	idleCtx     context.Context

	forceExit       atomic.Bool
	doneEmmitted    atomic.Int32
	idleGotCalled   atomic.Bool
	idleStatusStack struct {
		s  []Status
		mu sync.RWMutex
	}

	// InterruptSignal -.
	InterruptSignal chan os.Signal = make(chan os.Signal, 1)
)

func init() {
	signal.Notify(InterruptSignal, os.Interrupt, syscall.SIGTERM)
	idleStatusStack.s = []Status{StatusIdle}
}

// DoTerminate forces immediate termination of the application.
func DoTerminate(force bool) {
	forceExit.Store(force || IsAppIdling())

	slog.With(
		"force", force,
		"forceExit", forceExit.Load(),
	).Debug("Immediate termination status")
}

// GetBusy registers a process as busy, to prevent idle timeout.
func GetBusy(is Status) {
	if is == StatusIdle {
		return
	}

	slog.Debug("server got a lot busier", "is", is)

	idleStatusStack.mu.Lock()
	idleStatusStack.s = append(idleStatusStack.s, is)
	idleStatusStack.mu.Unlock()

	if idleGotCalled.Load() {
		idleCancel()
	}
}

// GetFree registers a process as less busy.
func GetFree(is Status) {
	logw := slog.With("is", is)

	idleStatusStack.mu.Lock()
	defer idleStatusStack.mu.Unlock()

	if is != StatusIdle {
		logw.Debug("server got a little less busy")

		for i := len(idleStatusStack.s) - 1; i >= 0; i-- {
			if is == idleStatusStack.s[i] {
				idleStatusStack.s[i] = idleStatusStack.s[len(idleStatusStack.s)-1]
				idleStatusStack.s = idleStatusStack.s[:len(idleStatusStack.s)-1]
				break
			}
		}
	}

	logw.Debug("Topmost idle status", "status", idleStatusStack.s[len(idleStatusStack.s)-1])

	if len(idleStatusStack.s) == 1 {
		if !idleGotCalled.Load() {
			idleCtx, idleCancel = context.WithCancel(context.Background())
			go Idle(idleCtx)
		}
	}
}

// Idle exits the server if it has been idle for a while and no long-term
// processes are pending.
func Idle(ctx context.Context) {
	idleStatusStack.mu.RLock()
	logw := slog.With(
		"forceExit", forceExit.Load(),
		"len(idleStatusStack)", len(idleStatusStack.s)-1,
	)
	idleStatusStack.mu.RUnlock()

	logw.Info("Starting Idle checks")

	if !forceExit.Load() {
		if IsAppBusy() || idleGotCalled.Load() {
			logw.Info("Server is busy or already idling, so cancelling request")
			<-ctx.Done()
			return
		}

		idleGotCalled.Store(true)
		logw.Info("Entering Idle state")

		select {
		case <-time.After(time.Duration(ServerIdleTimeout) * time.Second):
			if IsAppBusy() {
				logw.Info("Server is busy, so cancelling timeout")
				<-ctx.Done()
				return
			}
			break
		case <-ctx.Done():
			logw.Info("idleCancel got called explicitly")
			idleGotCalled.Store(false)
			return
		}
	}

	if doneEmmitted.Load() > 0 {
		logw.Warn("ignoring further attempt at ctx.Done()", "doneEmmitted", doneEmmitted.Load())

		doneEmmitted.Add(1)
		return
	}

	doneEmmitted.Add(1)

	logw.Info("Server seems to have been Idle for a while, and that's gotta stop!")
	InterruptSignal <- os.Interrupt
}

// IsAppBusy returns true if some process has registered as busy.
func IsAppBusy() bool {
	idleStatusStack.mu.RLock()
	defer idleStatusStack.mu.RUnlock()

	return len(idleStatusStack.s) > 1
}

// IsAppBusyBy returns true if some process has registered as busy.
func IsAppBusyBy(is Status) bool {
	idleStatusStack.mu.RLock()
	defer idleStatusStack.mu.RUnlock()

	return slices.Contains(idleStatusStack.s, is)
}

// IsAppIdling returns true if the Idle method is active.
func IsAppIdling() bool {
	idleStatusStack.mu.RLock()
	defer idleStatusStack.mu.RUnlock()

	return idleGotCalled.Load() || len(idleStatusStack.s) == 1
}

// Start -.
func Start(serverTimeout ...int) {
	if idleStarted {
		return
	}

	if len(serverTimeout) > 0 {
		if serverTimeout[0] > DefaultServerIdleTimeout {
			ServerIdleTimeout = serverTimeout[0]
		}
	}
	GetFree(StatusIdle)

	idleStarted = true
}
