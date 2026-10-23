package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

// envPrefix starts the environment variable for each flag:
// -max-peers can be set with TORRENT_AI_MAX_PEERS.
const envPrefix = "TORRENT_AI_"

// registry wraps a flag.FlagSet with what the help text and startup
// checks need: groups, spec-defined defaults and minimum values.
type registry struct {
	fs       *flag.FlagSet
	groups   []*flagGroup
	current  *flagGroup
	info     map[string]*flagInfo
	checks   []func() error
	usageArg string
	notes    string                      // printed under the usage line
	lookup   func(string) (string, bool) // os.LookupEnv, swappable in tests
	fromEnv  []string                    // flags set from the environment
}

type flagGroup struct {
	title string
	names []string
}

type flagInfo struct {
	kind   string // placeholder shown in help, e.g. "duration"
	def    string // default, as the flag prints it
	spec   string // non-empty if a spec defines the default
	secret bool   // value is redacted from logs
}

func newRegistry(name, usageArg string) *registry {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	return &registry{fs: fs, info: map[string]*flagInfo{}, usageArg: usageArg, lookup: os.LookupEnv}
}

// group starts a new section in the help text.
func (r *registry) group(title string) {
	r.current = &flagGroup{title: title}
	r.groups = append(r.groups, r.current)
}

// add registers v. spec names the specification that defines the
// default, if any; changing such a value is flagged at startup.
func (r *registry) add(v flag.Value, name, kind, usage, spec string) {
	r.fs.Var(v, name, usage)
	r.info[name] = &flagInfo{kind: kind, def: v.String(), spec: spec}
	r.current.names = append(r.current.names, name)
}

func (r *registry) str(p *string, name, usage string) {
	r.add((*stringValue)(p), name, "string", usage, "")
}

func (r *registry) boolean(p *bool, name, usage string) {
	r.add((*boolValue)(p), name, "", usage, "")
}

// integer registers an int flag that must be at least minimum.
func (r *registry) integer(p *int, name string, minimum int, usage, spec string) {
	r.add((*intValue)(p), name, "int", usage, spec)
	r.checks = append(r.checks, func() error {
		if *p < minimum {
			return fmt.Errorf("-%s must be at least %d, got %d", name, minimum, *p)
		}
		return nil
	})
}

// duration registers a duration flag that must be at least minimum.
func (r *registry) duration(p *time.Duration, name string, minimum time.Duration, usage, spec string) {
	r.add((*durationValue)(p), name, "duration", usage, spec)
	r.checks = append(r.checks, func() error {
		if *p < minimum {
			return fmt.Errorf("-%s must be at least %v, got %v", name, minimum, *p)
		}
		return nil
	})
}

// size registers a byte size flag ("16KiB", "2MiB", "1G") that must be
// at least minimum.
func size[T int | int64](r *registry, p *T, name string, minimum T, usage, spec string) {
	r.add(&sizeValue[T]{p}, name, "size", usage, spec)
	r.checks = append(r.checks, func() error {
		if *p < minimum {
			return fmt.Errorf("-%s must be at least %s, got %s", name, formatSize(int64(minimum)), formatSize(int64(*p)))
		}
		return nil
	})
}

// list registers a flag that may be repeated or given comma-separated
// values. Setting it replaces the default list.
func (r *registry) list(p *[]string, name, usage string) {
	r.add(&listValue{p: p}, name, "list", usage, "")
}

// envName returns the environment variable for a flag.
func envName(flagName string) string {
	return envPrefix + strings.ToUpper(strings.ReplaceAll(flagName, "-", "_"))
}

// parse applies environment variables, then args, which take
// precedence, and runs the minimum-value checks.
func (r *registry) parse(args []string) error {
	r.fromEnv = nil
	for _, g := range r.groups {
		for _, name := range g.names {
			v, ok := r.lookup(envName(name))
			if !ok {
				continue
			}
			// A switch is turned on by its variable being present,
			// even if empty.
			if v == "" && isBoolFlag(r.fs.Lookup(name).Value) {
				v = "true"
			}
			if err := r.fs.Set(name, v); err != nil {
				return fmt.Errorf("invalid value %q for %s: %w", v, envName(name), err)
			}
			r.fromEnv = append(r.fromEnv, name)
		}
	}

	if err := r.fs.Parse(args); err != nil {
		return err
	}
	for _, check := range r.checks {
		if err := check(); err != nil {
			return err
		}
	}
	return nil
}

func isBoolFlag(v flag.Value) bool {
	b, ok := v.(interface{ IsBoolFlag() bool })
	return ok && b.IsBoolFlag()
}

// specOverrides returns the spec-defined flags set to something other
// than their default.
func (r *registry) specOverrides() []specOverride {
	var out []specOverride
	r.fs.Visit(func(f *flag.Flag) {
		info := r.info[f.Name]
		if info.spec != "" && f.Value.String() != info.def {
			out = append(out, specOverride{name: f.Name, value: f.Value.String(), def: info.def, spec: info.spec})
		}
	})
	return out
}

type specOverride struct {
	name, value, def, spec string
}

// secret marks a flag whose value must not be logged, such as one
// carrying credentials.
func (r *registry) secret(name string) {
	r.info[name].secret = true
}

// values returns every flag's effective value, in help order, for
// logging. Secret values are redacted.
func (r *registry) values() []any {
	var attrs []any
	for _, g := range r.groups {
		for _, name := range g.names {
			v := r.fs.Lookup(name).Value.String()
			if r.info[name].secret && v != "" {
				v = "(redacted)"
			}
			attrs = append(attrs, name, v)
		}
	}
	return attrs
}

// printUsage writes the grouped help text.
func (r *registry) printUsage(w io.Writer) {
	fmt.Fprintf(w, "usage: %s [flags] %s\n", r.fs.Name(), r.usageArg)
	if r.notes != "" {
		fmt.Fprint(w, r.notes)
	}

	for _, g := range r.groups {
		fmt.Fprintf(w, "\n%s:\n", g.title)
		for _, name := range g.names {
			f := r.fs.Lookup(name)
			info := r.info[name]

			head := "  -" + name
			if strings.HasPrefix(info.kind, "[") {
				head += info.kind
			} else if info.kind != "" {
				head += " " + info.kind
			}
			fmt.Fprintln(w, head)

			usage := f.Usage
			if info.def != "" && info.def != "false" && info.def != "[]" {
				usage += fmt.Sprintf(" (default %s)", info.def)
			}
			if info.spec != "" {
				usage += fmt.Sprintf(" [spec-defined recommended default: %s]", info.spec)
			}
			fmt.Fprintf(w, "    \t%s\n", usage)
		}
	}

	fmt.Fprint(w, `
Flags marked [spec-defined recommended default] use the value a BitTorrent
specification (BEP) defines or recommends. They can be changed for testing,
but other clients may disconnect or misbehave; the client logs a warning
when one is overridden.

Every flag can also be set by an environment variable named TORRENT_AI_
plus the flag name in upper case with - as _, for example
TORRENT_AI_MAX_PEERS=20 or TORRENT_AI_DHT=false. A switch such as
TORRENT_AI_FLAME is turned on just by being set, even to an empty value.
Flags on the command line take precedence.

Sizes accept B, KiB, MiB and GiB (K, M, G and KB, MB, GB mean the same,
powers of 1024). Durations use Go syntax: 500ms, 30s, 5m, 1h.

Exit codes: 0 complete, 1 error, 2 usage, 3 timeout, 130 interrupted
(SIGINT), 143 terminated (SIGTERM). Either signal shuts down cleanly,
writing profiles and flushing telemetry; a second one exits at once.
`)
}

type stringValue string

func (v *stringValue) Set(s string) error { *v = stringValue(s); return nil }
func (v *stringValue) String() string     { return string(*v) }

type boolValue bool

func (v *boolValue) Set(s string) error {
	b, err := strconv.ParseBool(s)
	*v = boolValue(b)
	return err
}
func (v *boolValue) String() string   { return strconv.FormatBool(bool(*v)) }
func (v *boolValue) IsBoolFlag() bool { return true }

type intValue int

func (v *intValue) Set(s string) error {
	n, err := strconv.Atoi(s)
	*v = intValue(n)
	return err
}
func (v *intValue) String() string { return strconv.Itoa(int(*v)) }

type durationValue time.Duration

func (v *durationValue) Set(s string) error {
	d, err := time.ParseDuration(s)
	*v = durationValue(d)
	return err
}
func (v *durationValue) String() string { return time.Duration(*v).String() }

type sizeValue[T int | int64] struct{ p *T }

func (v *sizeValue[T]) Set(s string) error {
	n, err := parseSize(s)
	if err != nil {
		return err
	}
	*v.p = T(n)
	return nil
}
func (v *sizeValue[T]) String() string { return formatSize(int64(*v.p)) }

type listValue struct {
	p   *[]string
	set bool // false until the first Set, which drops the default
}

func (v *listValue) Set(s string) error {
	if !v.set {
		*v.p = nil
		v.set = true
	}
	for _, item := range strings.Split(s, ",") {
		if item = strings.TrimSpace(item); item != "" {
			*v.p = append(*v.p, item)
		}
	}
	return nil
}

func (v *listValue) String() string {
	if v.p == nil {
		return "[]"
	}
	return "[" + strings.Join(*v.p, ",") + "]"
}

var sizeUnits = []struct {
	suffixes []string
	n        int64
}{
	{[]string{"GiB", "GB", "G"}, 1 << 30},
	{[]string{"MiB", "MB", "M"}, 1 << 20},
	{[]string{"KiB", "KB", "K"}, 1 << 10},
	{[]string{"B", ""}, 1},
}

// parseSize parses a byte count such as "16KiB", "2M" or "4096".
func parseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	for _, u := range sizeUnits {
		for _, suffix := range u.suffixes {
			if !strings.HasSuffix(strings.ToUpper(s), strings.ToUpper(suffix)) {
				continue
			}
			num := strings.TrimSpace(s[:len(s)-len(suffix)])
			n, err := strconv.ParseInt(num, 10, 64)
			if err != nil {
				return 0, fmt.Errorf("invalid size %q", s)
			}
			return n * u.n, nil
		}
	}
	return 0, fmt.Errorf("invalid size %q", s)
}

// formatSize renders a byte count in the largest unit that divides it
// exactly.
func formatSize(n int64) string {
	for _, u := range sizeUnits[:3] {
		if n != 0 && n%u.n == 0 {
			return fmt.Sprintf("%d%s", n/u.n, u.suffixes[0])
		}
	}
	return strconv.FormatInt(n, 10)
}
