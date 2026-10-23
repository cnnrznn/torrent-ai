package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	httppprof "net/http/pprof"
	"os"
	"runtime"
	"runtime/pprof"
	"strings"
	"time"

	"github.com/cnnrznn/torrent-ai/pkg/pyroscope"
)

// defaultFlameAddr is where -flame serves profiles when no address is
// given. It's loopback only: profiles expose goroutine stacks and
// other internals, so serving them to other hosts is opt-in.
const defaultFlameAddr = "localhost:6060"

// profiling holds the -flame and -pyroscope settings.
type profiling struct {
	addr          string // pprof HTTP address; empty means off
	cpuFile       string
	heapFile      string
	blockRate     int
	mutexFraction int

	pyroscope pyroscopeConfig
}

// pyroscopeConfig holds the -pyroscope flags; uploads run when url is
// set.
type pyroscopeConfig struct {
	url                string
	app                string
	labels             []string // key=value
	interval           time.Duration
	user, password     string
	tenant             string
	caFile             string
	insecureSkipVerify bool
}

// uploader builds the Pyroscope uploader, or returns nil if no URL is
// set.
func (c pyroscopeConfig) uploader(log *slog.Logger) (*pyroscope.Uploader, error) {
	if c.url == "" {
		return nil, nil
	}
	labels := pyroscope.DefaultLabels(version())
	for _, kv := range c.labels {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return nil, fmt.Errorf("-pyroscope-labels: %q is not key=value", kv)
		}
		labels[k] = v
	}
	return pyroscope.New(pyroscope.Config{
		URL:                c.url,
		App:                c.app,
		Labels:             labels,
		Interval:           c.interval,
		User:               c.user,
		Password:           c.password,
		TenantID:           c.tenant,
		CAFile:             c.caFile,
		InsecureSkipVerify: c.insecureSkipVerify,
		Logger:             log,
	})
}

// switchAddr is a flag that is either off or an address. Given no value
// (-flame, or TORRENT_AI_FLAME set to "", "1" or "true") it takes def;
// "false", "0" or "off" turn it off; anything else is the address.
type switchAddr struct {
	p   *string
	def string
}

func (v *switchAddr) Set(s string) error {
	switch strings.ToLower(s) {
	case "", "true", "1", "on", "yes":
		*v.p = v.def
	case "false", "0", "off", "no":
		*v.p = ""
	default:
		*v.p = s
	}
	return nil
}

func (v *switchAddr) String() string {
	if v.p == nil {
		return ""
	}
	return *v.p
}

func (v *switchAddr) IsBoolFlag() bool { return true }

// start enables the requested profiling. The returned function stops
// it and writes any profile files; call it once the run is over.
func (p profiling) start(ctx context.Context, log *slog.Logger) (stop func(), err error) {
	runtime.SetBlockProfileRate(p.blockRate)
	runtime.SetMutexProfileFraction(p.mutexFraction)

	var stops []func()
	stop = func() {
		for i := len(stops) - 1; i >= 0; i-- {
			stops[i]()
		}
	}

	if p.cpuFile != "" {
		f, err := os.Create(p.cpuFile)
		if err != nil {
			return stop, fmt.Errorf("-flame-cpu-file: %w", err)
		}
		if err := pprof.StartCPUProfile(f); err != nil {
			f.Close()
			return stop, fmt.Errorf("-flame-cpu-file: %w", err)
		}
		log.Info("cpu profile started", "file", p.cpuFile)
		stops = append(stops, func() {
			pprof.StopCPUProfile()
			f.Close()
			log.Info("cpu profile written", "file", p.cpuFile, "hint", "go tool pprof -http=: "+p.cpuFile)
		})
	}

	if p.heapFile != "" {
		stops = append(stops, func() {
			if err := writeHeapProfile(p.heapFile); err != nil {
				log.Error("heap profile failed", "file", p.heapFile, "err", err)
				return
			}
			log.Info("heap profile written", "file", p.heapFile, "hint", "go tool pprof -http=: "+p.heapFile)
		})
	}

	if p.addr != "" {
		if p.cpuFile != "" {
			log.Warn("flame cpu profile unavailable over http",
				"reason", "-flame-cpu-file holds Go's only CPU profiler for the whole run",
				"hint", "use one or the other; heap, goroutine, block and mutex profiles still work")
		}
		ln, err := net.Listen("tcp", p.addr)
		if err != nil {
			stop()
			return func() {}, fmt.Errorf("-flame: %w", err)
		}
		srv := &http.Server{Handler: pprofMux(), ReadHeaderTimeout: 10 * time.Second}
		go srv.Serve(ln)

		url := "http://" + ln.Addr().String() + "/debug/pprof"
		log.Info("flame endpoint listening",
			"url", url,
			"block_rate", p.blockRate,
			"mutex_fraction", p.mutexFraction,
			"hint", "go tool pprof -http=: "+url+"/profile?seconds=30",
		)
		stops = append(stops, func() {
			ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
			defer cancel()
			srv.Shutdown(ctx)
		})
	}

	return stop, nil
}

// pprofMux serves the standard /debug/pprof handlers without touching
// http.DefaultServeMux.
func pprofMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", httppprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", httppprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", httppprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", httppprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", httppprof.Trace)
	return mux
}

func writeHeapProfile(path string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	runtime.GC() // so the profile reflects live objects
	return errors.Join(pprof.WriteHeapProfile(f), f.Close())
}

// labeled runs fn with a pprof "component" label, which goroutines it
// starts inherit, so flame graphs can be filtered by subsystem.
func labeled(ctx context.Context, component string, fn func(context.Context)) {
	pprof.Do(ctx, pprof.Labels("component", component), fn)
}
