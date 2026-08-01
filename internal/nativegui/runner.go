package nativegui

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"altv1/internal/application"
	"altv1/internal/event"
)

var (
	hostSequence atomic.Uint64
	hostMu       sync.RWMutex
	hosts        = make(map[uint64]*Host)
)

func Run(
	ctx context.Context,
	app *application.Application,
	launch Launch,
	updates ...io.Reader,
) (*Published, error) {
	host, err := NewHost(ctx, app, launch)
	if err != nil {
		return nil, err
	}
	handle := hostSequence.Add(1)
	hostMu.Lock()
	hosts[handle] = host
	hostMu.Unlock()
	defer func() {
		hostMu.Lock()
		delete(hosts, handle)
		hostMu.Unlock()
	}()
	if launch.Mode == ModeThinking && len(updates) > 0 && updates[0] != nil {
		go receiveEvents(ctx, handle, host, updates[0])
		go reconcileEvents(ctx, handle, host)
	}

	// winit requires the native event loop to be created and driven on the
	// process main thread. The hidden GUI process enters Run before starting
	// any other GUI work, so pin this goroutine for the entire window lifetime.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if code := runNative(handle); code != 0 {
		return nil, fmt.Errorf("native GUI exited with code %d", code)
	}
	return host.Published(), nil
}

func receiveEvents(ctx context.Context, handle uint64, host *Host, source io.Reader) {
	decoder := json.NewDecoder(source)
	for {
		var item event.Event
		if err := decoder.Decode(&item); err != nil {
			if err != io.EOF && ctx.Err() == nil {
				host.SetStreamError(fmt.Errorf("decode live thinking event: %w", err))
				wakeNative(handle)
			}
			return
		}
		if ctx.Err() != nil {
			return
		}
		if err := host.PushEvent(item); err != nil {
			host.SetStreamError(fmt.Errorf("apply live thinking event: %w", err))
			wakeNative(handle)
			continue
		}
		host.SetStreamError(nil)
		wakeNative(handle)
	}
}

func reconcileEvents(ctx context.Context, handle uint64, host *Host) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			changed, err := host.Reconcile()
			if err != nil {
				wakeNative(handle)
				continue
			}
			if changed {
				wakeNative(handle)
			}
		}
	}
}

func exchangeForHandle(handle uint64, request []byte, capacity int) []byte {
	hostMu.RLock()
	host := hosts[handle]
	hostMu.RUnlock()
	if host == nil {
		return mustJSON(Response{OK: false, Error: "native GUI host is no longer available"})
	}
	return host.ExchangeSized(request, capacity)
}
