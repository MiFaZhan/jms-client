package obs

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

// fixedEvent returns an event with every field set, so a test can prove that
// each one survives the trip to disk.
func fixedEvent() Event {
	code := 0
	return Event{
		TS:          time.Date(2026, 10, 5, 14, 32, 1, 123000000, time.FixedZone("CST", 8*3600)),
		PID:         12345,
		Actor:       "mcp:pi",
		Kind:        KindExecEnd,
		Server:      "prod",
		Asset:       "web-01",
		Backend:     "ssh",
		Endpoint:    "external",
		Command:     "systemctl status nginx",
		ExitCode:    &code,
		DurationMS:  118,
		OutputBytes: 2140,
		Preview:     "nginx.service active",
		Pool:        "hit",
		IdleMS:      68000,
		Reason:      "idle timeout",
		Error:       "",
	}
}

// readLines returns the file's non-empty lines.
func readLines(t *testing.T, path string) []string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open audit file: %v", err)
	}
	defer f.Close()
	var lines []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		if line := sc.Text(); line != "" {
			lines = append(lines, line)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan audit file: %v", err)
	}
	return lines
}

// decodeLine unmarshals one audit line.
func decodeLine(t *testing.T, line string) Event {
	t.Helper()
	var e Event
	if err := json.Unmarshal([]byte(line), &e); err != nil {
		t.Fatalf("line is not valid JSON (%v): %q", err, line)
	}
	return e
}

func TestAuditWriterWritesOneJSONObjectPerLine(t *testing.T) {
	dir := t.TempDir()
	w, err := NewAuditWriter(AuditOptions{Dir: dir})
	if err != nil {
		t.Fatalf("NewAuditWriter: %v", err)
	}

	first := fixedEvent()
	second := fixedEvent()
	second.Kind = KindExecStart
	second.ExitCode = nil
	second.Preview = ""
	w.Publish(first)
	w.Publish(second)
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	lines := readLines(t, w.Path())
	if len(lines) != 2 {
		t.Fatalf("file has %d lines, want 2: %q", len(lines), lines)
	}
	got := decodeLine(t, lines[0])
	// The writer owns the per-pid file and stamps its own pid, so a fixture
	// pid must not survive into the line.
	if got.PID != os.Getpid() {
		t.Errorf("pid = %d, want the writer's pid %d", got.PID, os.Getpid())
	}
	want := first
	want.PID = os.Getpid()
	if !got.TS.Equal(want.TS) {
		t.Errorf("ts = %s, want %s", got.TS.Format(time.RFC3339Nano), want.TS.Format(time.RFC3339Nano))
	}
	if got.Actor != want.Actor || got.Kind != want.Kind || got.Server != want.Server ||
		got.Asset != want.Asset || got.Backend != want.Backend || got.Endpoint != want.Endpoint ||
		got.Command != want.Command || got.DurationMS != want.DurationMS ||
		got.OutputBytes != want.OutputBytes || got.Preview != want.Preview ||
		got.Pool != want.Pool || got.IdleMS != want.IdleMS || got.Reason != want.Reason {
		t.Errorf("round trip lost fields:\n got %+v\nwant %+v", got, want)
	}
	if got.ExitCode == nil || *got.ExitCode != 0 {
		t.Errorf("exit_code = %v, want 0", got.ExitCode)
	}

	secondGot := decodeLine(t, lines[1])
	if secondGot.ExitCode != nil {
		t.Errorf("nil exit_code was written as %v, want omitted", *secondGot.ExitCode)
	}
	if secondGot.Preview != "" {
		t.Errorf("empty preview was written as %q, want omitted", secondGot.Preview)
	}
}

func TestAuditFilePathMatchesPerPidNaming(t *testing.T) {
	dir := t.TempDir()
	before := time.Now().Format("20060102")
	w, err := NewAuditWriter(AuditOptions{Dir: dir})
	if err != nil {
		t.Fatalf("NewAuditWriter: %v", err)
	}
	t.Cleanup(func() { w.Close() })
	after := time.Now().Format("20060102")

	name := filepath.Base(w.Path())
	re := regexp.MustCompile(`^audit-(\d{8})-(\d+)\.jsonl$`)
	m := re.FindStringSubmatch(name)
	if m == nil {
		t.Fatalf("file name %q does not match audit-YYYYMMDD-<pid>.jsonl", name)
	}
	if m[1] != before && m[1] != after {
		t.Errorf("file name date = %s, want today (%s)", m[1], before)
	}
	if want := fmt.Sprint(os.Getpid()); m[2] != want {
		t.Errorf("file name pid = %s, want %s", m[2], want)
	}
	// The file must exist in the requested directory, not merely be named as
	// if it did.
	if got, want := filepath.Dir(w.Path()), filepath.Clean(dir); got != want {
		t.Errorf("file directory = %s, want %s", got, want)
	}
	if _, err := os.Stat(w.Path()); err != nil {
		t.Errorf("audit file does not exist: %v", err)
	}
}

func TestAuditWriterTruncatesPreview(t *testing.T) {
	dir := t.TempDir()
	const limit = 32
	w, err := NewAuditWriter(AuditOptions{Dir: dir, PreviewBytes: limit})
	if err != nil {
		t.Fatalf("NewAuditWriter: %v", err)
	}
	long := strings.Repeat("x", limit*3)
	w.Publish(Event{Kind: KindExecEnd, OutputBytes: len(long), Preview: long})
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	got := decodeLine(t, readLines(t, w.Path())[0])
	if len(got.Preview) != limit {
		t.Errorf("preview length = %d, want %d", len(got.Preview), limit)
	}
	// Truncation must not misreport the size of the output it describes.
	if got.OutputBytes != len(long) {
		t.Errorf("output_bytes = %d, want %d", got.OutputBytes, len(long))
	}
}

func TestAuditWriterKeepsPreviewAtTheLimitAndWhenFull(t *testing.T) {
	dir := t.TempDir()
	const limit = 16
	w, err := NewAuditWriter(AuditOptions{Dir: dir, PreviewBytes: limit, Full: true})
	if err != nil {
		t.Fatalf("NewAuditWriter: %v", err)
	}
	exact := strings.Repeat("y", limit)
	over := strings.Repeat("z", limit*4)
	w.Publish(Event{Kind: KindExecEnd, Preview: exact})
	w.Publish(Event{Kind: KindExecEnd, Preview: over})
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	lines := readLines(t, w.Path())
	if got := decodeLine(t, lines[0]).Preview; got != exact {
		t.Errorf("preview at the limit = %q, want it untouched", got)
	}
	if got := decodeLine(t, lines[1]).Preview; got != over {
		t.Errorf("Full preview length = %d, want %d (untruncated)", len(got), len(over))
	}
}

func TestAuditWriterDoesNotSplitUTF8InPreview(t *testing.T) {
	dir := t.TempDir()
	const limit = 5
	w, err := NewAuditWriter(AuditOptions{Dir: dir, PreviewBytes: limit})
	if err != nil {
		t.Fatalf("NewAuditWriter: %v", err)
	}
	w.Publish(Event{Kind: KindExecEnd, Preview: strings.Repeat("é", 10)}) // 2 bytes per rune
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	got := decodeLine(t, readLines(t, w.Path())[0]).Preview
	if len(got) > limit {
		t.Errorf("preview length = %d, want at most %d", len(got), limit)
	}
	if !utf8.ValidString(got) || got != strings.Repeat("é", 2) {
		t.Errorf("preview = %q, want two whole runes", got)
	}
}

func TestAuditWriterCloseIsIdempotentAndFileStaysReadable(t *testing.T) {
	w, err := NewAuditWriter(AuditOptions{Dir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewAuditWriter: %v", err)
	}
	w.Publish(Event{Kind: KindExecStart, Command: "before close"})
	if err := w.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	// Publishing after close must be a no-op, not a panic or a write to a
	// closed descriptor.
	w.Publish(Event{Kind: KindExecStart, Command: "after close"})

	lines := readLines(t, w.Path())
	if len(lines) != 1 {
		t.Fatalf("file has %d lines after close, want 1", len(lines))
	}
	if got := decodeLine(t, lines[0]).Command; got != "before close" {
		t.Errorf("command = %q, want before close", got)
	}
}

func TestNilAuditWriterIsInert(t *testing.T) {
	// NewBusWithAudit returns a nil writer when auditing is off, and callers
	// close it unconditionally.
	var w *AuditWriter
	w.Publish(Event{Kind: KindExecStart})
	if err := w.Close(); err != nil {
		t.Errorf("nil Close = %v, want nil", err)
	}
	if got := w.Path(); got != "" {
		t.Errorf("nil Path = %q, want empty", got)
	}
}

func TestAuditWriterAppendsAcrossReopen(t *testing.T) {
	// Two writers for the same pid and day must not truncate each other: the
	// per-pid file is opened with O_APPEND.
	dir := t.TempDir()
	first, err := NewAuditWriter(AuditOptions{Dir: dir})
	if err != nil {
		t.Fatalf("NewAuditWriter: %v", err)
	}
	first.Publish(Event{Kind: KindExecStart, Command: "one"})
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	second, err := NewAuditWriter(AuditOptions{Dir: dir})
	if err != nil {
		t.Fatalf("NewAuditWriter: %v", err)
	}
	second.Publish(Event{Kind: KindExecStart, Command: "two"})
	if err := second.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if second.Path() != first.Path() {
		t.Fatalf("reopen used %s, want %s", second.Path(), first.Path())
	}

	lines := readLines(t, second.Path())
	if len(lines) != 2 {
		t.Fatalf("file has %d lines, want 2", len(lines))
	}
	if got := decodeLine(t, lines[0]).Command; got != "one" {
		t.Errorf("first line = %q, want one", got)
	}
	if got := decodeLine(t, lines[1]).Command; got != "two" {
		t.Errorf("second line = %q, want two", got)
	}
}

func TestConcurrentPublishWritesWholeLines(t *testing.T) {
	// The bus delivers from one goroutine, but an AuditWriter is a
	// Subscriber that any caller may hold, so its append must serialize.
	const goroutines = 16
	const each = 25
	w, err := NewAuditWriter(AuditOptions{Dir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewAuditWriter: %v", err)
	}

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				w.Publish(Event{Kind: KindExecEnd, Command: fmt.Sprintf("g%02d-%03d", n, i)})
			}
		}(g)
	}
	wg.Wait()
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	lines := readLines(t, w.Path())
	if len(lines) != goroutines*each {
		t.Fatalf("file has %d lines, want %d: concurrent appends interleaved or were lost",
			len(lines), goroutines*each)
	}
	seen := make(map[string]int, goroutines*each)
	for _, line := range lines {
		e := decodeLine(t, line)
		if e.Kind != KindExecEnd {
			t.Fatalf("line carries kind %q, want %q: %q", e.Kind, KindExecEnd, line)
		}
		seen[e.Command]++
	}
	for g := 0; g < goroutines; g++ {
		for i := 0; i < each; i++ {
			cmd := fmt.Sprintf("g%02d-%03d", g, i)
			if seen[cmd] != 1 {
				t.Errorf("command %q appears %d times, want exactly 1", cmd, seen[cmd])
			}
		}
	}
}

func TestBusPublishWithinQueueCapacityLosesNothing(t *testing.T) {
	// The queue is the contract that makes non-blocking delivery acceptable:
	// a burst no larger than subscriberBuffer must arrive in full, so a drop
	// always means a genuinely slow subscriber rather than ordinary load.
	bus, w, err := NewBusWithAudit(context.Background(), AuditOptions{Dir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewBusWithAudit: %v", err)
	}

	const goroutines = 8
	const each = subscriberBuffer / goroutines
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				bus.Publish(Event{Kind: KindExecEnd, Command: fmt.Sprintf("g%02d-%03d", n, i)})
			}
		}(g)
	}
	wg.Wait()

	want := goroutines * each
	waitForLines(t, w.Path(), want)
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := bus.Dropped(); got != 0 {
		t.Errorf("Dropped() = %d, want 0 for a burst within queue capacity (%d)", got, subscriberBuffer)
	}
	if got := len(readLines(t, w.Path())); got != want {
		t.Errorf("file has %d lines, want %d", got, want)
	}
}

func TestConcurrentBusPublishWritesWholeLines(t *testing.T) {
	// Concurrent Publish calls on the bus, through the audit subscriber, must
	// produce exactly one valid line each with no interleaving.
	const goroutines = 16
	const each = 25
	bus, w, err := NewBusWithAudit(context.Background(), AuditOptions{Dir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewBusWithAudit: %v", err)
	}

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				bus.Publish(Event{Kind: KindExecEnd, Command: fmt.Sprintf("g%02d-%03d", n, i)})
			}
		}(g)
	}
	wg.Wait()
	// Publish only enqueues, so the subscriber's queue may still hold events.
	// Dropped is final once every publisher has returned.
	want := goroutines*each - int(bus.Dropped())
	waitForLines(t, w.Path(), want)
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// The subscriber queue is bounded and Publish never blocks, so a burst
	// larger than the queue is allowed to drop. What must hold is that every
	// line on disk is a whole, parseable record and that none is duplicated
	// or torn.
	lines := readLines(t, w.Path())
	if len(lines) == 0 {
		t.Fatal("no events reached the audit file")
	}
	seen := make(map[string]int, len(lines))
	for _, line := range lines {
		e := decodeLine(t, line)
		if e.Kind != KindExecEnd {
			t.Fatalf("line carries kind %q, want %q: %q", e.Kind, KindExecEnd, line)
		}
		seen[e.Command]++
	}
	for cmd, n := range seen {
		if n != 1 {
			t.Errorf("command %q appears %d times, want exactly 1", cmd, n)
		}
	}
	if len(lines) != want {
		t.Errorf("file has %d lines, want %d (published minus %d dropped)",
			len(lines), want, bus.Dropped())
	}
}

func TestMarshalLineUsesTheDocumentedJSONKeys(t *testing.T) {
	// A marshal/unmarshal round trip cannot catch a renamed tag: both sides
	// would agree on the wrong name. The keys are the on-disk contract shared
	// with `jms tail` and `jms log show`, so pin them literally.
	code := 0
	line, err := MarshalLine(Event{
		TS:          time.Date(2026, 10, 5, 14, 32, 1, 123000000, time.UTC),
		PID:         12345,
		Actor:       "mcp:pi",
		Kind:        KindExecEnd,
		Server:      "prod",
		Asset:       "web-01",
		Backend:     "ssh",
		Endpoint:    "external",
		Command:     "df -h",
		ExitCode:    &code,
		DurationMS:  118,
		OutputBytes: 2140,
		Preview:     "Filesystem",
		Pool:        "hit",
		IdleMS:      68000,
		Reason:      "reused",
		Error:       "none",
	})
	if err != nil {
		t.Fatalf("MarshalLine: %v", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(line, &raw); err != nil {
		t.Fatalf("unmarshal into a map: %v", err)
	}

	want := []string{
		"ts", "pid", "actor", "kind", "server", "asset", "backend",
		"endpoint", "command", "exit_code", "duration_ms", "output_bytes",
		"preview", "pool", "idle_ms", "reason", "error",
	}
	if len(raw) != len(want) {
		t.Errorf("line has %d keys, want %d: %s", len(raw), len(want), line)
	}
	for _, key := range want {
		if _, ok := raw[key]; !ok {
			t.Errorf("missing key %q in %s", key, line)
		}
	}
	// The kind values are the enumeration consumers filter on.
	for _, k := range []Kind{KindExecStart, KindExecEnd, KindPoolCold, KindPoolHit,
		KindPoolEvict, KindPoolReap, KindSessionLogin, KindSessionRelogin,
		KindEndpointSelect, KindError} {
		encoded, err := MarshalLine(Event{Kind: k})
		if err != nil {
			t.Fatalf("MarshalLine(%s): %v", k, err)
		}
		if !strings.Contains(string(encoded), `"kind":"`+string(k)+`"`) {
			t.Errorf("kind %q is not written verbatim: %s", k, encoded)
		}
	}
}

func TestMarshalLineRoundTripPreservesEveryField(t *testing.T) {
	in := fixedEvent()
	line, err := MarshalLine(in)
	if err != nil {
		t.Fatalf("MarshalLine: %v", err)
	}
	if strings.Contains(string(line), "\n") {
		t.Fatalf("MarshalLine emitted a newline: %q", line)
	}
	out := decodeLine(t, string(line))

	if !out.TS.Equal(in.TS) {
		t.Errorf("ts = %s, want %s", out.TS, in.TS)
	}
	if out.PID != in.PID || out.Actor != in.Actor || out.Kind != in.Kind ||
		out.Server != in.Server || out.Asset != in.Asset || out.Backend != in.Backend ||
		out.Endpoint != in.Endpoint || out.Command != in.Command ||
		out.DurationMS != in.DurationMS || out.OutputBytes != in.OutputBytes ||
		out.Preview != in.Preview || out.Pool != in.Pool || out.IdleMS != in.IdleMS ||
		out.Reason != in.Reason || out.Error != in.Error {
		t.Errorf("round trip lost fields:\n got %+v\nwant %+v", out, in)
	}
	if out.ExitCode == nil || *out.ExitCode != *in.ExitCode {
		t.Errorf("exit_code = %v, want %d", out.ExitCode, *in.ExitCode)
	}
}

func TestMarshalLineCarriesTheErrorField(t *testing.T) {
	// The error text is the record of a failed execution; it must survive the
	// trip to disk. A credential must never be put in it in the first place
	// (shared rules §7), which is why nothing here needs redacting.
	line, err := MarshalLine(Event{Kind: KindError, Error: "dial tcp 10.0.0.5:2222: connection refused"})
	if err != nil {
		t.Fatalf("MarshalLine: %v", err)
	}
	out := decodeLine(t, string(line))
	if out.Error != "dial tcp 10.0.0.5:2222: connection refused" {
		t.Errorf("error = %q, want the original text", out.Error)
	}
}

func TestMarshalLineOmitsNilExitCode(t *testing.T) {
	line, err := MarshalLine(Event{Kind: KindExecStart, Command: "ls"})
	if err != nil {
		t.Fatalf("MarshalLine: %v", err)
	}
	if strings.Contains(string(line), "exit_code") {
		t.Errorf("nil exit_code was not omitted: %s", line)
	}
	// A zero exit code is a real result and must be present.
	zero := 0
	line, err = MarshalLine(Event{Kind: KindExecEnd, ExitCode: &zero})
	if err != nil {
		t.Fatalf("MarshalLine: %v", err)
	}
	if !strings.Contains(string(line), `"exit_code":0`) {
		t.Errorf("zero exit_code was omitted: %s", line)
	}
}

func TestNewAuditWriterWithoutDirUsesConfigDir(t *testing.T) {
	// JMS_AUDIT_DIR is what relocates the default; config.ConfigDir honours
	// JMS_CONFIG, which is the CLI's injected-config-path route.
	base := t.TempDir()
	t.Setenv("JMS_CONFIG", filepath.Join(base, "config.toml"))

	w, err := NewAuditWriter(AuditOptions{})
	if err != nil {
		t.Fatalf("NewAuditWriter: %v", err)
	}
	t.Cleanup(func() { w.Close() })

	if want := filepath.Join(base, "audit"); filepath.Dir(w.Path()) != want {
		t.Errorf("default audit directory = %s, want %s", filepath.Dir(w.Path()), want)
	}
}

func TestNewBusWithAuditWiresTheWriter(t *testing.T) {
	dir := t.TempDir()
	bus, w, err := NewBusWithAudit(context.Background(), AuditOptions{Dir: dir})
	if err != nil {
		t.Fatalf("NewBusWithAudit: %v", err)
	}
	if w == nil {
		t.Fatal("writer is nil, want an enabled writer")
	}

	bus.Publish(Event{Kind: KindExecStart, Command: "ls /"})
	// The bus delivers asynchronously, so the file may lag the publish; poll
	// for the observable effect rather than sleeping a fixed amount.
	// Closing first would race the delivery, so close only after it lands.
	waitForLines(t, w.Path(), 1)
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	lines := readLines(t, w.Path())
	if len(lines) != 1 {
		t.Fatalf("file has %d lines, want 1", len(lines))
	}
	if got := decodeLine(t, lines[0]).Command; got != "ls /" {
		t.Errorf("command = %q, want ls /", got)
	}
}

func TestNewBusWithAuditOffDisablesTheWriter(t *testing.T) {
	t.Setenv(EnvAudit, "off")
	dir := t.TempDir()

	bus, w, err := NewBusWithAudit(context.Background(), AuditOptions{Dir: dir})
	if err != nil {
		t.Fatalf("NewBusWithAudit: %v", err)
	}
	if w != nil {
		t.Errorf("writer = %+v, want nil when %s=off", w, EnvAudit)
	}
	if bus == nil {
		t.Fatal("bus is nil, want a working bus even with auditing off")
	}
	// The bus must still work: attach and `--last` replay do not depend on
	// the audit file.
	sub := newCollecting()
	bus.Subscribe(sub)
	bus.Publish(Event{Kind: KindExecStart, Command: "observed"})
	if got := sub.take(t).Command; got != "observed" {
		t.Errorf("command = %q, want observed", got)
	}
	if entries, err := os.ReadDir(dir); err != nil {
		t.Fatalf("read audit dir: %v", err)
	} else if len(entries) != 0 {
		t.Errorf("audit directory holds %d entries with auditing off, want 0", len(entries))
	}
	if err := w.Close(); err != nil {
		t.Errorf("Close on a disabled writer = %v, want nil", err)
	}
}

func TestNewBusWithAuditRelocatesTheDirectory(t *testing.T) {
	t.Setenv(EnvAuditDir, "")
	relocated := filepath.Join(t.TempDir(), "elsewhere")
	t.Setenv(EnvAuditDir, relocated)

	bus, w, err := NewBusWithAudit(context.Background(), AuditOptions{})
	if err != nil {
		t.Fatalf("NewBusWithAudit: %v", err)
	}
	t.Cleanup(func() { w.Close() })
	if w == nil {
		t.Fatal("writer is nil, want an enabled writer")
	}
	if dir := filepath.Dir(w.Path()); dir != relocated {
		t.Fatalf("audit directory = %s, want %s", dir, relocated)
	}

	bus.Publish(Event{Kind: KindExecStart, Command: "relocated"})
	waitForLines(t, w.Path(), 1)
	entries, err := os.ReadDir(relocated)
	if err != nil {
		t.Fatalf("read relocated dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("relocated directory holds %d entries, want the per-pid file", len(entries))
	}
}

func TestNewBusWithAuditHonoursFullAndPreviewBytes(t *testing.T) {
	t.Setenv(EnvAuditFull, "1")
	t.Setenv(EnvAuditPreviewBytes, "8")

	_, w, err := NewBusWithAudit(context.Background(), AuditOptions{Dir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewBusWithAudit: %v", err)
	}
	if !w.full || w.previewBytes != 8 {
		t.Errorf("writer = full:%v previewBytes:%d, want full:true previewBytes:8", w.full, w.previewBytes)
	}
	w.Publish(Event{Kind: KindExecEnd, Preview: strings.Repeat("p", 64)})
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := decodeLine(t, readLines(t, w.Path())[0]).Preview; len(got) != 64 {
		t.Errorf("preview length = %d, want 64: %s=1 must win over %s", len(got), EnvAuditFull, EnvAuditPreviewBytes)
	}
}

func TestNewBusWithAuditPreviewBytesEnvTruncates(t *testing.T) {
	t.Setenv(EnvAuditPreviewBytes, "12")

	_, w, err := NewBusWithAudit(context.Background(), AuditOptions{Dir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewBusWithAudit: %v", err)
	}
	w.Publish(Event{Kind: KindExecEnd, Preview: strings.Repeat("q", 100)})
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := decodeLine(t, readLines(t, w.Path())[0]).Preview; len(got) != 12 {
		t.Errorf("preview length = %d, want 12", len(got))
	}
}

func TestNewBusWithAuditIgnoresMalformedPreviewBytes(t *testing.T) {
	// A typo in the tunable must not disable auditing or crash the process.
	t.Setenv(EnvAuditPreviewBytes, "not-a-number")

	_, w, err := NewBusWithAudit(context.Background(), AuditOptions{Dir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewBusWithAudit: %v", err)
	}
	if w == nil {
		t.Fatal("writer is nil, want an enabled writer")
	}
	w.Publish(Event{Kind: KindExecEnd, Preview: strings.Repeat("r", DefaultPreviewBytes+1)})
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := decodeLine(t, readLines(t, w.Path())[0]).Preview; len(got) != DefaultPreviewBytes {
		t.Errorf("preview length = %d, want DefaultPreviewBytes (%d)", len(got), DefaultPreviewBytes)
	}
}

func TestNewAuditWriterFailsWhenTheDirectoryCannotBeCreated(t *testing.T) {
	// A file where the directory should be is the portable way to force a
	// failure on every platform.
	base := t.TempDir()
	blocker := filepath.Join(base, "audit")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("write blocker: %v", err)
	}

	if _, err := NewAuditWriter(AuditOptions{Dir: blocker}); err == nil {
		t.Fatal("NewAuditWriter succeeded on a directory that is a regular file")
	}
}

func TestNewBusWithAuditReportsAnUnusableDirectory(t *testing.T) {
	base := t.TempDir()
	blocker := filepath.Join(base, "audit")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("write blocker: %v", err)
	}

	bus, w, err := NewBusWithAudit(context.Background(), AuditOptions{Dir: blocker})
	if err == nil {
		t.Fatal("NewBusWithAudit succeeded on an unusable directory")
	}
	if w != nil {
		t.Errorf("writer = %+v, want nil on failure", w)
	}
	// The bus survives so the caller can keep observing over IPC and report
	// the audit problem without failing the command.
	if bus == nil {
		t.Fatal("bus is nil, want a usable bus even when auditing failed")
	}
}

func TestAuditFilesAreOwnerOnlyOnUnix(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows governs access with ACLs, not file modes")
	}
	dir := filepath.Join(t.TempDir(), "audit")
	w, err := NewAuditWriter(AuditOptions{Dir: dir})
	if err != nil {
		t.Fatalf("NewAuditWriter: %v", err)
	}
	t.Cleanup(func() { w.Close() })

	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat audit dir: %v", err)
	}
	if got := dirInfo.Mode().Perm(); got != 0o700 {
		t.Errorf("audit directory mode = %04o, want 0700", got)
	}
	fileInfo, err := os.Stat(w.Path())
	if err != nil {
		t.Fatalf("stat audit file: %v", err)
	}
	if got := fileInfo.Mode().Perm(); got != 0o600 {
		t.Errorf("audit file mode = %04o, want 0600", got)
	}
}

func TestAuditWriterTightensPreexistingPermissionsOnUnix(t *testing.T) {
	// A directory or file left world-readable by an earlier run must not
	// keep leaking output previews (DESIGN.md §11.7).
	if runtime.GOOS == "windows" {
		t.Skip("Windows governs access with ACLs, not file modes")
	}
	dir := filepath.Join(t.TempDir(), "audit")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Create the exact per-pid file the writer will open, with a loose mode.
	name := fmt.Sprintf("audit-%s-%d.jsonl", time.Now().Format("20060102"), os.Getpid())
	if err := os.WriteFile(filepath.Join(dir, name), nil, 0o644); err != nil {
		t.Fatalf("write pre-existing file: %v", err)
	}

	w, err := NewAuditWriter(AuditOptions{Dir: dir})
	if err != nil {
		t.Fatalf("NewAuditWriter: %v", err)
	}
	t.Cleanup(func() { w.Close() })

	if info, err := os.Stat(dir); err != nil {
		t.Fatalf("stat dir: %v", err)
	} else if got := info.Mode().Perm(); got != 0o700 {
		t.Errorf("audit directory mode = %04o, want 0700", got)
	}
	if info, err := os.Stat(w.Path()); err != nil {
		t.Fatalf("stat file: %v", err)
	} else if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("audit file mode = %04o, want 0600", got)
	}
}

// waitForLines polls path until it holds at least want non-empty lines.
//
// The bus delivers asynchronously, so a test that publishes through it must
// wait for the observable effect instead of assuming it happened inside
// Publish.
func waitForLines(t *testing.T, path string, want int) {
	t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for {
		if len(readLines(t, path)) >= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("audit file %s never reached %d line(s)", path, want)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestAuditWriterResolvesTheDirectoryIntoPath(t *testing.T) {
	// An empty AuditOptions.Dir must resolve to <config_dir>/audit, and Path
	// must report the resolved file, not an empty string.
	base := t.TempDir()
	t.Setenv("JMS_CONFIG", filepath.Join(base, "config.toml"))

	w, err := NewAuditWriter(AuditOptions{})
	if err != nil {
		t.Fatalf("NewAuditWriter: %v", err)
	}
	t.Cleanup(func() { w.Close() })

	if w.dir != filepath.Join(base, "audit") {
		t.Errorf("resolved dir = %q, want %q", w.dir, filepath.Join(base, "audit"))
	}
	if filepath.Dir(w.Path()) != w.dir {
		t.Errorf("Path() directory = %q, want the resolved dir %q", filepath.Dir(w.Path()), w.dir)
	}
}
