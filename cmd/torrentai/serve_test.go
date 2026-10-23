package main

import (
	"bufio"
	"bytes"
	"crypto/sha1"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/cnnrznn/torrent-ai/pkg/bencoding"
	"github.com/cnnrznn/torrent-ai/pkg/daemon"
	"github.com/cnnrznn/torrent-ai/pkg/metainfo"
)

// shortTempDir returns a temp dir with a short path, since unix socket
// paths are limited to about 100 bytes.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "tai")
	require.NoError(t, err)
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// syncBuffer is a buffer one goroutine writes while another reads.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) WriteLine(line []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.b.Write(line)
	s.b.WriteByte('\n')
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// daemonProc is a running torrentai serve.
type daemonProc struct {
	cmd      *exec.Cmd
	stateDir string
	logs     *syncBuffer
	exited   chan struct{}
}

func startDaemon(t *testing.T, bin, stateDir, downloadDir string, port int) *daemonProc {
	t.Helper()
	cmd := exec.Command(bin, "serve",
		"-state-dir", stateDir, "-o", downloadDir,
		"-port", strconv.Itoa(port), "-dht=false", "-log-format", "json",
		"-status-interval", "1s")
	logs := &syncBuffer{}
	stderr, err := cmd.StderrPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())

	ready := make(chan struct{})
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			logs.WriteLine(sc.Bytes())
			if strings.Contains(sc.Text(), `"msg":"api listening"`) {
				select {
				case <-ready:
				default:
					close(ready)
				}
			}
		}
	}()
	select {
	case <-ready:
	case <-time.After(15 * time.Second):
		cmd.Process.Kill()
		t.Fatalf("daemon didn't start:\n%s", logs.String())
	}
	d := &daemonProc{cmd: cmd, stateDir: stateDir, logs: logs, exited: exited}
	t.Cleanup(func() { d.stop(t) })
	return d
}

func (d *daemonProc) stop(t *testing.T) {
	if d.cmd.ProcessState != nil {
		return
	}
	d.cmd.Process.Signal(syscall.SIGTERM)
	<-d.exited
	d.cmd.Wait()
}

// client runs a client command against the daemon and returns stdout.
func (d *daemonProc) client(t *testing.T, args ...string) string {
	t.Helper()
	var out bytes.Buffer
	name := args[0]
	args = append([]string{"-state-dir", d.stateDir}, args[1:]...)
	code := clientMain(name, args, &out)
	require.Equal(t, exitComplete, code, "%s %v: %s", name, args, out.String())
	return out.String()
}

func (d *daemonProc) list(t *testing.T) []daemon.Status {
	t.Helper()
	var list []daemon.Status
	require.NoError(t, json.Unmarshal([]byte(d.client(t, "ls", "-json")), &list))
	return list
}

func (d *daemonProc) waitState(t *testing.T, want daemon.State) daemon.Status {
	t.Helper()
	var last []daemon.Status
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		last = d.list(t)
		if len(last) == 1 && last[0].State == want {
			return last[0]
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("no torrent reached %s; last %+v\nlogs:\n%s", want, last, d.logs.String())
	return daemon.Status{}
}

func writeTestTorrent(t *testing.T, dir string, size int) (torrentFile string, meta *metainfo.Metainfo, data []byte) {
	t.Helper()
	r := rand.New(rand.NewPCG(9, 9))
	data = make([]byte, size)
	for i := range data {
		data[i] = byte(r.UintN(256))
	}
	const pieceLength = 16 * 1024
	var pieces []byte
	for off := 0; off < size; off += pieceLength {
		h := sha1.Sum(data[off:min(off+pieceLength, size)])
		pieces = append(pieces, h[:]...)
	}
	info := bencoding.EncodeDictionary(map[string]string{
		"name":         bencoding.EncodeString("data.bin"),
		"length":       bencoding.EncodeInteger(size),
		"piece length": bencoding.EncodeInteger(pieceLength),
		"pieces":       bencoding.EncodeString(string(pieces)),
	})
	file := bencoding.EncodeDictionary(map[string]string{"info": info})
	meta, err := metainfo.New(file)
	require.NoError(t, err)
	torrentFile = filepath.Join(dir, "data.torrent")
	require.NoError(t, os.WriteFile(torrentFile, []byte(file), 0o644))
	return torrentFile, meta, data
}

// TestServe runs two daemons as real processes: one seeds a .torrent,
// the other downloads it from its magnet link, then restarts and
// restores it.
func TestServe(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the binary")
	}
	bin := buildBinary(t)

	seedState, seedDir := shortTempDir(t), shortTempDir(t)
	torrentFile, meta, data := writeTestTorrent(t, seedDir, 300_000)
	require.NoError(t, os.WriteFile(filepath.Join(seedDir, "data.bin"), data, 0o644))

	seedPort := freePort(t)
	seeder := startDaemon(t, bin, seedState, seedDir, seedPort)
	seeder.client(t, "add", torrentFile)
	seeder.waitState(t, daemon.StateSeeding)

	leechState, leechDir := shortTempDir(t), shortTempDir(t)
	leecher := startDaemon(t, bin, leechState, leechDir, freePort(t))
	magnet := "magnet:?xt=urn:btih:" + meta.InfoHash().Hex()
	out := leecher.client(t, "add", "-peer", fmt.Sprintf("127.0.0.1:%d", seedPort), magnet)
	require.Contains(t, out, meta.InfoHash().Hex())

	st := leecher.waitState(t, daemon.StateSeeding)
	require.Equal(t, "data.bin", st.Name)
	got, err := os.ReadFile(filepath.Join(leechDir, "data.bin"))
	require.NoError(t, err)
	require.Equal(t, data, got)

	// The text views render.
	require.Contains(t, leecher.client(t, "ls"), "seeding")
	require.Regexp(t, `progress:\s+100\.0%`, leecher.client(t, "status", meta.InfoHash().Hex()))

	// Restart: the torrent is restored without fetching metadata.
	leecher.stop(t)
	require.Contains(t, leecher.logs.String(), `"msg":"exit"`)
	leecher = startDaemon(t, bin, leechState, leechDir, freePort(t))
	st = leecher.waitState(t, daemon.StateSeeding)
	require.Equal(t, "data.bin", st.Name)
	require.Contains(t, leecher.logs.String(), `"msg":"torrents restored","component":"session","count":1`)
	require.NotContains(t, leecher.logs.String(), `"msg":"metadata fetched"`)

	leecher.client(t, "rm", "-delete-files", meta.InfoHash().Hex())
	require.Empty(t, leecher.list(t))
	require.NoFileExists(t, filepath.Join(leechDir, "data.bin"))
}
