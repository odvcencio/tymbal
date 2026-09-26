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
