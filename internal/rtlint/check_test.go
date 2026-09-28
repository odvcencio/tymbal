package rtlint

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCheckerFindsRealtimeViolations(t *testing.T) {
	root := t.TempDir()
	source := `package fixture
import (
    "fmt"
    "time"
)
func helper() {}
//tymbal:rt
func bad(ch chan int, lock interface{ Lock() }) {
    make([]byte, 1)
    go helper()
    ch <- 1
    <-ch
    select { default: }
    defer helper()
    fmt.Println("bad")
    time.Sleep(1)
    helper()
    lock.Lock()
    _ = "bad" + " call"
}
`
	if err := os.WriteFile(filepath.Join(root, "fixture.go"), []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	issues, err := Check(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 13 {
		t.Fatalf("got %d issues, want 13: %v", len(issues), issues)
	}
	for _, issue := range issues {
		if issue.Position.Line == 0 || !strings.HasSuffix(issue.Position.Filename, "fixture.go") {
			t.Fatalf("missing source position: %v", issue)
		}
	}
}

func TestRepositoryRealtimeMarkers(t *testing.T) {
	_, file, _, _ := runtime.Caller(0)
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "../.."))
	issues, err := Check(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, issue := range issues {
		t.Error(issue)
	}
}

func TestCheckerCoversRealtimeRulesIndividually(t *testing.T) {
	tests := []struct {
		name, source, want string
	}{
		{"make", "package p\n//tymbal:rt\nfunc bad(){ _=make([]byte,1) }", "make on real-time path"},
		{"new", "package p\n//tymbal:rt\nfunc bad(){ _=new(int) }", "new on real-time path"},
		{"append", "package p\n//tymbal:rt\nfunc bad(b []byte){ _=append(b,1) }", "append on real-time path"},
		{"escaping composite", "package p\ntype item struct{}\n//tymbal:rt\nfunc bad() *item { p:=&item{}; return p }", "escaping composite literal"},
		{"escaping local array address", "package p\n//tymbal:rt\nfunc bad() *[1]int { values:=[1]int{1}; return &values }", "escaping composite literal"},
		{"escaping slice literal", "package p\n//tymbal:rt\nfunc bad() []int { return []int{1} }", "escaping composite literal"},
		{"string concatenation", "package p\n//tymbal:rt\nfunc bad(a,b string) string { return a+b }", "string concatenation"},
		{"global string concatenation", "package p\nconst left=\"left\"\nconst right=\"right\"\n//tymbal:rt\nfunc bad() string { return left+right }", "string concatenation"},
		{"byte to string", "package p\n//tymbal:rt\nfunc bad(b []byte) string { return string(b) }", "byte-to-string conversion"},
		{"named byte slice to string", "package p\ntype bytes []byte\n//tymbal:rt\nfunc bad(b bytes) string { return string(b) }", "byte-to-string conversion"},
		{"string to bytes", "package p\n//tymbal:rt\nfunc bad(s string) []byte { return []byte(s) }", "string-to-byte conversion"},
		{"named string to bytes", "package p\ntype text string\n//tymbal:rt\nfunc bad(s text) []byte { return []byte(s) }", "string-to-byte conversion"},
		{"goroutine", "package p\n//tymbal:rt\nfunc bad(){ go func(){}() }", "goroutine on real-time path"},
		{"channel send", "package p\n//tymbal:rt\nfunc bad(ch chan int){ ch<-1 }", "channel send on real-time path"},
		{"channel receive", "package p\n//tymbal:rt\nfunc bad(ch chan int){ _=<-ch }", "channel receive on real-time path"},
		{"channel close", "package p\n//tymbal:rt\nfunc bad(ch chan int){ close(ch) }", "channel operation on real-time path"},
		{"channel range alias", "package p\ntype events chan int\n//tymbal:rt\nfunc bad(ch events){ for range ch {} }", "channel receive on real-time path"},
		{"select", "package p\n//tymbal:rt\nfunc bad(ch chan int){ select { case <-ch: default: } }", "select on real-time path"},
		{"defer", "package p\nfunc cleanup(){}\n//tymbal:rt\nfunc bad(){ defer cleanup() }", "defer on real-time path"},
		{"fmt", "package p\nimport \"fmt\"\n//tymbal:rt\nfunc bad(){ fmt.Print(\"x\") }", "forbidden call fmt.Print"},
		{"log", "package p\nimport \"log\"\n//tymbal:rt\nfunc bad(){ log.Print(\"x\") }", "forbidden call log.Print"},
		{"errors.New", "package p\nimport \"errors\"\n//tymbal:rt\nfunc bad(){ _=errors.New(\"x\") }", "forbidden call errors.New"},
		{"time.Sleep", "package p\nimport \"time\"\n//tymbal:rt\nfunc bad(){ time.Sleep(1) }", "forbidden call time.Sleep"},
		{"time.After", "package p\nimport \"time\"\n//tymbal:rt\nfunc bad(){ _=time.After(1) }", "forbidden call time.After"},
		{"variadic procedure call", "package p\nimport \"syscall\"\n//tymbal:rt\nfunc bad(proc *syscall.LazyProc){ proc.Call(1) }", "variadic procedure Call on real-time path"},
		{"mutex type", "package p\nimport \"sync\"\n//tymbal:rt\nfunc bad(){ var _ sync.Mutex }", "sync mutex type"},
		{"mutex operation", "package p\n//tymbal:rt\nfunc bad(m interface{Lock()}){ m.Lock() }", "lock operation"},
		{"map read", "package p\n//tymbal:rt\nfunc bad(m map[int]int){ _=m[0] }", "map access"},
		{"named map read", "package p\ntype values map[int]int\n//tymbal:rt\nfunc bad(m values){ _=m[0] }", "map access"},
		{"map field read", "package p\ntype state struct{ values map[int]int }\n//tymbal:rt\nfunc bad(s *state){ _=s.values[0] }", "map access"},
		{"map write", "package p\n//tymbal:rt\nfunc bad(m map[int]int){ m[0]=1 }", "map access"},
		{"map iteration", "package p\n//tymbal:rt\nfunc bad(m map[int]int){ for range m {} }", "map access"},
		{"map mutation", "package p\n//tymbal:rt\nfunc bad(m map[int]int){ delete(m,0) }", "map access"},
		{"map literal", "package p\n//tymbal:rt\nfunc bad(){ _=map[int]int{} }", "map literal allocation"},
		{"unmarked local call", "package p\nfunc helper(){}\n//tymbal:rt\nfunc bad(){ helper() }", "call to unmarked local function helper"},
		{"unmarked local method", "package p\ntype worker struct{}\nfunc (*worker) helper(){}\n//tymbal:rt\nfunc bad(w *worker){ w.helper() }", "call to unmarked local method helper"},
		{"non-allowlisted package", "package p\nimport \"strings\"\n//tymbal:rt\nfunc bad(s string){ _=strings.TrimSpace(s) }", "call outside real-time allowlist strings.TrimSpace"},
		{"unsafe pointer storage", "package p\nimport \"unsafe\"\ntype holder struct{ buf uintptr }\n//tymbal:rt\nfunc bad(h *holder,b []byte){ h.buf=uintptr(unsafe.Pointer(&b[0])) }", "OS-visible pointer storage must reference arena memory"},
		{"unsafe pointer alias storage", "package p\nimport u \"unsafe\"\ntype holder struct{ ptr uintptr }\n//tymbal:rt\nfunc bad(h *holder,b []byte){ p:=uintptr(u.Pointer(&b[0])); h.ptr=p }", "OS-visible pointer storage must reference arena memory"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			issues := checkerIssues(t, tt.source)
			for _, issue := range issues {
				if strings.Contains(issue.Message, tt.want) {
					return
				}
			}
			t.Fatalf("issues %v do not include %q", issues, tt.want)
		})
	}
}

func TestCheckerAllowsRecoverWrapperSafeArenaAndAllowlist(t *testing.T) {
	source := `package p
import (
    "math"
    "sync/atomic"
    "unsafe"
)
type holder struct { arena []byte; data []byte; buf uintptr }
//tymbal:rt
func callCallback(cb func()) (panicked bool) {
    defer func() { if recover()!=nil { panicked=true } }()
    cb()
    return false
}
//tymbal:rt
func publish(h *holder, p *uint64) float64 {
    h.buf=uintptr(unsafe.Pointer(&h.data[0]))
    _=atomic.LoadUint64(p)
    _=unsafe.Sizeof(p)
    return math.Abs(-1)
}
//tymbal:rt
func helper(){}
//tymbal:rt
func caller(){ helper() }
`
	issues := checkerIssues(t, source)
	if len(issues) != 0 {
		t.Fatalf("safe recover, arena, allowlisted, or marked calls produced issues: %v", issues)
	}
}

func TestCheckerRequiresMarkersForImportedModuleCalls(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.com/tymbal\n"), 0600); err != nil {
		t.Fatal(err)
	}
	helperDir := filepath.Join(root, "helper")
	if err := os.Mkdir(helperDir, 0700); err != nil {
		t.Fatal(err)
	}
	caller := `package fixture
import "example.com/tymbal/helper"
//tymbal:rt
func call() { helper.Run() }
`
	helper := "package helper\nfunc Run() {}\n"
	write := func(path, source string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(source), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(root, "fixture.go"), caller)
	helperPath := filepath.Join(helperDir, "helper.go")
	write(helperPath, helper)
	issues, err := Check(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 1 || !strings.Contains(issues[0].Message, "call to unmarked imported function example.com/tymbal/helper.Run") {
		t.Fatalf("issues %v do not report the unmarked imported call", issues)
	}

	write(helperPath, "package helper\n//tymbal:rt\nfunc Run() {}\n")
	issues, err = Check(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 0 {
		t.Fatalf("marked imported function produced issues: %v", issues)
	}
}

func TestCheckerAllowsNonEscapingCompositeShadowingGlobal(t *testing.T) {
	source := `package p
type item struct{}
var local *item
//tymbal:rt
func safe() { local := &item{}; _ = local }
`
	if issues := checkerIssues(t, source); len(issues) != 0 {
		t.Fatalf("non-escaping local composite produced issues: %v", issues)
	}
}

func checkerIssues(t *testing.T, source string) []Issue {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "fixture.go"), []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	issues, err := Check(root)
	if err != nil {
		t.Fatal(err)
	}
	return issues
}
