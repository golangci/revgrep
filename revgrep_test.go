package revgrep

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
)

func setup(t *testing.T, stage, subdir string) []byte {
	t.Helper()

	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("could not get working dir: %s", err)
	}

	testDataDir := filepath.Join(wd, "testdata")

	// Execute make
	cmd := exec.CommandContext(t.Context(), "bash", "./make.sh", stage)
	cmd.Dir = testDataDir

	gitOutput, err := cmd.CombinedOutput()
	if err != nil {
		t.Logf("%s: git setup: %s", stage, string(gitOutput))
		t.Fatalf("could not run make.sh: %v", err)
	}

	gitDir := filepath.Join(testDataDir, "git")

	t.Cleanup(func() {
		_ = os.RemoveAll(gitDir)
	})

	cmd = exec.CommandContext(t.Context(), "go", "vet", "./...")
	cmd.Dir = gitDir

	goVetOutput, err := cmd.CombinedOutput()
	if cmd.ProcessState.ExitCode() != 1 {
		t.Logf("%s: go vet: %s", stage, string(goVetOutput))
		t.Fatalf("could not run go vet: %v", err)
	}

	// chdir so the vcs exec commands read the correct testdata
	t.Chdir(filepath.Join(gitDir, subdir))

	if stage == "11-abs-path" {
		goVetOutput = regexp.MustCompile(`(.+\.go)`).
			ReplaceAll(goVetOutput, []byte(filepath.Join(gitDir, "$1")))
	}

	// clean go vet output
	goVetOutput = bytes.ReplaceAll(goVetOutput, []byte("."+string(filepath.Separator)), []byte(""))

	t.Logf("%s: go vet clean: %s", stage, string(goVetOutput))

	return goVetOutput
}

// Tests line matching and extraction of issue.
func TestChecker_Check_Regexp(t *testing.T) {
	tests := []struct {
		regexp string
		line   string
		want   Issue
	}{
		{
			line: "file.go:1:issue",
			want: Issue{File: "file.go", LineNo: 1, HunkPos: 2, Issue: "file.go:1:issue", Message: "issue"},
		},
		{
			line: "file.go:1:5:issue",
			want: Issue{File: "file.go", LineNo: 1, ColNo: 5, HunkPos: 2, Issue: "file.go:1:5:issue", Message: "issue"},
		},
		{
			line: "file.go:1:  issue",
			want: Issue{File: "file.go", LineNo: 1, HunkPos: 2, Issue: "file.go:1:  issue", Message: "issue"},
		},
		{
			regexp: `.*?:(.*?\.go):([0-9]+):()(.*)`,
			line:   "prefix:file.go:1:issue",
			want:   Issue{File: "file.go", LineNo: 1, HunkPos: 2, Issue: "prefix:file.go:1:issue", Message: "issue"},
		},
	}

	diff := []byte(`--- a/file.go
+++ b/file.go
@@ -1,1 +1,1 @@
-func Line() {}
+func NewLine() {}`)

	for _, test := range tests {
		checker := Checker{
			Patch:  bytes.NewReader(diff),
			Regexp: test.regexp,
		}

		issues, err := checker.Check(context.Background(), bytes.NewReader([]byte(test.line)), io.Discard)
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}

		want := []Issue{test.want}
		if !reflect.DeepEqual(issues, want) {
			t.Errorf("unexpected issues for line: %q\nhave: %#v\nwant: %#v", test.line, issues, want)
		}
	}
}

// Tests [Checker.WholeFiles] will report any issues in files that have changes,
// even if they are outside the diff.
func TestChecker_Check_WholeFiles(t *testing.T) {
	tests := []struct {
		name    string
		line    string
		matches bool
	}{
		{
			name:    "inside diff",
			line:    "file.go:1:issue",
			matches: true,
		},
		{
			name:    "outside diff",
			line:    "file.go:10:5:issue",
			matches: true,
		},
		{
			name: "different file",
			line: "file2.go:1:issue",
		},
	}

	diff := []byte(`--- a/file.go
+++ b/file.go
@@ -1,1 +1,1 @@
-func Line() {}
+func NewLine() {}`)

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			checker := Checker{
				Patch:      bytes.NewReader(diff),
				WholeFiles: true,
			}

			issues, err := checker.Check(context.Background(), bytes.NewReader([]byte(test.line)), io.Discard)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if test.matches && len(issues) != 1 {
				t.Fatalf("expected one issue to be returned, but got %#v", issues)
			}

			if !test.matches && len(issues) != 0 {
				t.Fatalf("expected no issues to be returned, but got %#v", issues)
			}
		})
	}
}

// Tests the writer in the argument to the Changes function
// and generally tests the entire program functionality.
func TestChecker_Check_changesWriter(t *testing.T) {
	tests := map[string]struct {
		subdir  string
		exp     []string // file:linenumber including trailing colon
		revFrom string
		revTo   string
	}{
		"2-untracked":            {exp: []string{"main.go:3:"}},
		"3-untracked-subdir":     {exp: []string{"main.go:3:", "subdir/main.go:3:"}},
		"3-untracked-subdir-cwd": {subdir: "subdir", exp: []string{"main.go:3:"}},
		"4-commit":               {exp: []string{"main.go:3:", "subdir/main.go:3:"}},
		"5-unstaged-no-warning":  {},
		"6-unstaged":             {exp: []string{"main.go:6:"}},
		// From a commit, all changes should be shown
		"7-commit": {exp: []string{"main.go:6:"}, revFrom: "HEAD~1"},
		// From a commit+unstaged, all changes should be shown
		"8-unstaged": {exp: []string{"main.go:6:", "main.go:7:"}, revFrom: "HEAD~1"},
		// From a commit+unstaged+untracked, all changes should be shown
		"9-untracked": {exp: []string{"main.go:6:", "main.go:7:", "main2.go:3:"}, revFrom: "HEAD~1"},
		// From a commit to last commit, all changes should be shown except recent unstaged, untracked
		"10-committed": {exp: []string{"main.go:6:"}, revFrom: "HEAD~1", revTo: "HEAD~0"},
		// static analysis tools with absolute paths should be handled
		"11-abs-path": {exp: []string{"main.go:6:"}, revFrom: "HEAD~1", revTo: "HEAD~0"},
		// Removing a single line shouldn't raise any issues.
		"12-removed-lines": {},
	}

	for stage, test := range tests {
		t.Run(stage, func(t *testing.T) {
			goVetOutput := setup(t, stage, test.subdir)

			var out bytes.Buffer

			c := Checker{
				RevisionFrom: test.revFrom,
				RevisionTo:   test.revTo,
			}

			_, err := c.Check(context.Background(), bytes.NewBuffer(goVetOutput), &out)
			if err != nil {
				t.Errorf("%s: unexpected error: %v", stage, err)
			}

			var lines []string

			scanner := bufio.NewScanner(&out)
			for scanner.Scan() {
				// Rewrite abs paths to for simpler matching
				line := rewriteAbs(t, scanner.Text())
				lines = append(lines, strings.TrimPrefix(line, "./"))
			}

			sort.Slice(lines, func(i, j int) bool {
				return lines[i] <= lines[j]
			})

			var count int
			for i, line := range lines {
				count++

				if i > len(test.exp)-1 {
					t.Errorf("%s: unexpected line: %q", stage, line)
				} else if !strings.HasPrefix(line, filepath.FromSlash(test.exp[i])) {
					t.Errorf("%s: line %q does not have prefix %q", stage, line, filepath.FromSlash(test.exp[i]))
				}
			}

			if count != len(test.exp) {
				t.Errorf("%s: got %d, expected %d", stage, count, len(test.exp))
			}
		})
	}
}

func TestChecker_linesChanged(t *testing.T) {
	diff := []byte(`--- a/file.go
+++ b/file.go
@@ -1,1 +1,1 @@
 // comment
-func Line() {}
+func NewLine() {}
@@ -20,1 +20,1 @@
 // comment
-func Line() {}
+func NewLine() {}
 // comment
@@ -3,1 +30,1 @@
-func Line() {}
+func NewLine() {}
 // comment`)

	checker := Checker{
		Patch: bytes.NewReader(diff),
	}

	have := checker.linesChanged()

	want := map[string][]pos{
		"file.go": {
			{lineNo: 2, hunkPos: 3},
			{lineNo: 21, hunkPos: 7},
			{lineNo: 30, hunkPos: 11},
		},
	}

	if !reflect.DeepEqual(have, want) {
		t.Errorf("unexpected pos:\nhave: %#v\nwant: %#v", have, want)
	}
}

func TestChecker_linesChanged_longLine(t *testing.T) {
	// ReadLine returns a line longer than its buffer in chunks: the tail of a long context line
	// must not be read as a removed line, an added line or a hunk header.
	for _, tail := range []string{"-y", "+y", "@@ y"} {
		diff := "--- a/file.go\n+++ b/file.go\n@@ -1,3 +1,4 @@\n a\n b\n " +
			strings.Repeat("x", 4095) + tail + "\n+func NewLine() {}\n"

		checker := Checker{
			Patch: strings.NewReader(diff),
		}

		have := checker.linesChanged()

		want := map[string][]pos{
			"file.go": {
				{lineNo: 4, hunkPos: 4},
			},
		}

		if !reflect.DeepEqual(have, want) {
			t.Errorf("tail %q: unexpected pos:\nhave: %#v\nwant: %#v", tail, have, want)
		}
	}
}

func TestChecker_linesChanged_longAddedLine(t *testing.T) {
	diff := "--- a/file.go\n+++ b/file.go\n@@ -1,1 +1,2 @@\n a\n+" + strings.Repeat("x", 5000) + "\n"

	checker := Checker{
		Patch: strings.NewReader(diff),
	}

	have := checker.linesChanged()

	want := map[string][]pos{
		"file.go": {
			{lineNo: 2, hunkPos: 2},
		},
	}

	if !reflect.DeepEqual(have, want) {
		t.Errorf("unexpected pos:\nhave: %#v\nwant: %#v", have, want)
	}
}

func TestChecker_linesChanged_noPrefix(t *testing.T) {
	// "git diff --no-prefix" writes the file name without "a/" and "b/".
	for name, want := range map[string]string{"a": "a", "x.go": "x.go", "b/file.go": "file.go", "w/file.go": "file.go"} {
		diff := "--- " + name + "\n+++ " + name + "\n@@ -1 +1 @@\n+x\n"

		checker := Checker{
			Patch: strings.NewReader(diff),
		}

		have := checker.linesChanged()

		if _, ok := have[want]; !ok || len(have) != 1 {
			t.Errorf("%q: unexpected files: %#v, want %q", name, have, want)
		}
	}
}

func rewriteAbs(t *testing.T, line string) string {
	t.Helper()

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}

	return strings.TrimPrefix(line, cwd+string(filepath.Separator))
}
