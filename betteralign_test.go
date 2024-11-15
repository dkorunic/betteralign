package betteralign_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dkorunic/betteralign"
	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/analysistest"
	"gotest.tools/v3/golden"
)

func NewTestAnalyzer() *analysis.Analyzer {
	analyzer := &analysis.Analyzer{
		Name:     betteralign.Analyzer.Name,
		Doc:      betteralign.Analyzer.Doc,
		Requires: betteralign.Analyzer.Requires,
		Run:      betteralign.Analyzer.Run,
	}
	betteralign.InitAnalyzer(analyzer)
	return analyzer
}

func TestSuggestions(t *testing.T) {
	testdata := analysistest.TestData()
	analyzer := NewTestAnalyzer()
	analysistest.Run(t, testdata, analyzer, "a")
}

func TestApply(t *testing.T) {
	srcDir := filepath.Join("testdata", "src")
	workDir := filepath.Join(srcDir, "a")

	tmpDir, err := os.MkdirTemp(srcDir, "apply-test")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	tmpWorkDir := filepath.Join(tmpDir, "a")

	if err := os.Mkdir(tmpWorkDir, 0o750); err != nil {
		t.Fatal(err)
	}

	paths, err := filepath.Glob(filepath.Join(workDir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}

	for _, path := range paths {
		testBasename := filepath.Base(path)
		testTmpname := filepath.Join(tmpWorkDir, testBasename)

		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(testTmpname, src, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	testdata := analysistest.TestData()

	analyzer := NewTestAnalyzer()
	analyzer.Flags.Set("apply", "true")

	analysistest.Run(t, testdata, analyzer, filepath.Join(filepath.Base(tmpDir), "a"))

	for _, path := range paths {
		testBasename := filepath.Base(path)
		testTmpname := filepath.Join(tmpWorkDir, testBasename)

		testResult, err := os.ReadFile(testTmpname)
		if err != nil {
			t.Fatal(err)
		}

		goldenFilename := filepath.Join("src", "a", strings.Join([]string{testBasename, ".golden"}, ""))
		golden.Assert(t, string(testResult), goldenFilename)
	}
}

func TestFlagExcludeDirs(t *testing.T) {
	t.Run("exclude none", func(t *testing.T) {
		testdata := analysistest.TestData()
		analyzer := NewTestAnalyzer()
		analyzer.Flags.Set("apply", "false")
		analysistest.Run(t, testdata, analyzer, "exclude/none/...")
	})

	t.Run("exclude all", func(t *testing.T) {
		testdata := analysistest.TestData()
		analyzer := NewTestAnalyzer()
		analyzer.Flags.Set("apply", "false")
		analyzer.Flags.Set("exclude_dirs", "testdata/src/exclude/all/")
		analysistest.Run(t, testdata, analyzer, "exclude/all/...")
	})

	t.Run("exclude a", func(t *testing.T) {
		testdata := analysistest.TestData()
		analyzer := NewTestAnalyzer()
		analyzer.Flags.Set("apply", "false")
		analyzer.Flags.Set("exclude_dirs", "testdata/src/exclude/a/a")
		analysistest.Run(t, testdata, analyzer, "exclude/a/...")
	})
}

func TestFlagExcludeFiles(t *testing.T) {
	t.Run("exclude b", func(t *testing.T) {
		testdata := analysistest.TestData()
		analyzer := NewTestAnalyzer()
		analyzer.Flags.Set("apply", "false")
		analyzer.Flags.Set("exclude_files", "testdata/src/exclude/b/b/*.go")
		analysistest.Run(t, testdata, analyzer, "exclude/b/...")
	})
}
