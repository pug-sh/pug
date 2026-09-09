package dotenv_test

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pug-sh/pug/internal/dotenv"
)

const subprocessEnv = "PUG_DOTENV_SUBPROCESS"

func writeEnv(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestLoadWithoutEnvFile(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := dotenv.Load(); err != nil {
		t.Fatalf("Load() = %v, want nil for an absent .env", err)
	}
}

func TestLoadAppliesValues(t *testing.T) {
	t.Chdir(writeEnv(t, "PUG_DOTENV_APPLIED=yes\n"))
	t.Cleanup(func() { _ = os.Unsetenv("PUG_DOTENV_APPLIED") })

	if err := dotenv.Load(); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("PUG_DOTENV_APPLIED"); got != "yes" {
		t.Fatalf("PUG_DOTENV_APPLIED = %q, want %q", got, "yes")
	}
}

func TestLoadDoesNotOverride(t *testing.T) {
	t.Chdir(writeEnv(t, "PUG_DOTENV_PRESET=from-file\n"))
	t.Setenv("PUG_DOTENV_PRESET", "from-environment")

	if err := dotenv.Load(); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("PUG_DOTENV_PRESET"); got != "from-environment" {
		t.Fatalf("PUG_DOTENV_PRESET = %q, want the exported value to win", got)
	}
}

func TestLoadMalformedEnvFile(t *testing.T) {
	// The eof cases have no trailing newline, where godotenv reports success.
	for name, body := range map[string]string{
		"unterminated quote":   "PUG_DOTENV_PARTIAL=1\nBROKEN=\"unterminated\n",
		"no assignment":        "PUG_DOTENV_PARTIAL=1\nnot an assignment\n",
		"no assignment at eof": "PUG_DOTENV_PARTIAL=1\nnot an assignment",
		"bare name at eof":     "PUG_DOTENV_PARTIAL",
	} {
		t.Run(name, func(t *testing.T) {
			t.Chdir(writeEnv(t, body))
			if err := dotenv.Load(); err == nil {
				t.Fatal("Load() = nil, want an error for a .env this broken")
			}
			if value, ok := os.LookupEnv("PUG_DOTENV_PARTIAL"); ok {
				t.Fatalf("PUG_DOTENV_PARTIAL = %q, want unset: a rejected .env must apply nothing", value)
			}
		})
	}
}

// A directory rather than chmod 000: the test user may be root.
func TestLoadUnreadableEnvFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".env"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	if err := dotenv.Load(); err == nil {
		t.Fatal("Load() = nil, want an error for an unreadable .env")
	}
}

// Only a real process shows LoadOrExit's exit status.
func runLoadOrExit(t *testing.T, dir string) (string, error) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestLoadOrExit$")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), subprocessEnv+"=1")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stderr.String(), err
}

func TestLoadOrExit(t *testing.T) {
	if os.Getenv(subprocessEnv) == "1" {
		dotenv.LoadOrExit(t.Context())
		return
	}

	t.Run("broken .env", func(t *testing.T) {
		stderr, err := runLoadOrExit(t, writeEnv(t, "BROKEN=\"unterminated\n"))

		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatalf("child exited with %v, want a non-zero status", err)
		}
		if got := exit.ExitCode(); got != 1 {
			t.Fatalf("child exit code = %d, want 1", got)
		}
		// A failing test also exits 1, so the log line is what pins the exit to LoadOrExit.
		if !strings.Contains(stderr, "failed to load .env") {
			t.Fatalf("child stderr = %q, want the .env failure logged", stderr)
		}
	})

	t.Run("no .env", func(t *testing.T) {
		if stderr, err := runLoadOrExit(t, t.TempDir()); err != nil {
			t.Fatalf("child exited with %v, want success; stderr = %q", err, stderr)
		}
	})
}
