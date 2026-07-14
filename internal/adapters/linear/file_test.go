package linear

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFileCredentialSourceReadsFixedPrivateTokenAndRotatesPerRequest(t *testing.T) {
	root, tokenPath := testCredentialRoot(t)
	if err := os.WriteFile(tokenPath, []byte("first-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	source := FileCredentialSource{Root: root}
	first, err := source.Resolve(context.Background(), FileCredentialSourceRef)
	if err != nil || first != "first-token" {
		t.Fatalf("first=%q err=%v", first, err)
	}
	if err := os.WriteFile(tokenPath, []byte("rotated-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := source.Resolve(context.Background(), FileCredentialSourceRef)
	if err != nil || second != "rotated-token" {
		t.Fatalf("second=%q err=%v", second, err)
	}
	if _, err := source.Resolve(context.Background(), EnvironmentCredentialSourceRef); err == nil {
		t.Fatal("file source accepted an environment reference")
	}
}

func TestFileCredentialSourceFailsClosedForUnsafeTopologyAndContent(t *testing.T) {
	for _, test := range []struct {
		name  string
		setup func(t *testing.T, root, tokenPath string)
	}{
		{name: "final symlink", setup: func(t *testing.T, root, tokenPath string) {
			t.Helper()
			target := filepath.Join(filepath.Dir(root), "target-token")
			if err := os.WriteFile(target, []byte("token"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, tokenPath); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "missing", setup: func(t *testing.T, _ string, _ string) { t.Helper() }},
		{name: "empty", setup: func(t *testing.T, _ string, tokenPath string) {
			t.Helper()
			if err := os.WriteFile(tokenPath, nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "group readable", setup: func(t *testing.T, _ string, tokenPath string) {
			t.Helper()
			if err := os.WriteFile(tokenPath, []byte("token"), 0o640); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(tokenPath, 0o640); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "hard link", setup: func(t *testing.T, root, tokenPath string) {
			t.Helper()
			other := filepath.Join(filepath.Dir(root), "linked-token")
			if err := os.WriteFile(other, []byte("token"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Link(other, tokenPath); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "multiple lines", setup: func(t *testing.T, _ string, tokenPath string) {
			t.Helper()
			if err := os.WriteFile(tokenPath, []byte("one\ntwo"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "nul byte", setup: func(t *testing.T, _ string, tokenPath string) {
			t.Helper()
			if err := os.WriteFile(tokenPath, []byte{'o', 0, 'k'}, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "oversize", setup: func(t *testing.T, _ string, tokenPath string) {
			t.Helper()
			if err := os.WriteFile(tokenPath, []byte(strings.Repeat("x", maxFileCredentialBytes+1)), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			root, tokenPath := testCredentialRoot(t)
			test.setup(t, root, tokenPath)
			_, err := (FileCredentialSource{Root: root}).Resolve(context.Background(), FileCredentialSourceRef)
			if err == nil || strings.Contains(err.Error(), root) || strings.Contains(err.Error(), "one") {
				t.Fatalf("unsafe source error=%v", err)
			}
		})
	}
}

func TestFileCredentialSourceRejectsUnsafeDirectoryAndReferenceInjection(t *testing.T) {
	root, tokenPath := testCredentialRoot(t)
	if err := os.WriteFile(tokenPath, []byte("token"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := (FileCredentialSource{Root: root}).Resolve(context.Background(), FileCredentialSourceRef); err == nil {
		t.Fatal("group-readable credential directory was accepted")
	}
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{
		"secret://file/linear-token/extra",
		"secret://file/../linear-token",
		"secret://file/linear-token?path=/tmp/token",
		"secret://file//linear-token",
		"/absolute/linear-token",
	} {
		if _, err := (FileCredentialSource{Root: root}).Resolve(context.Background(), ref); err == nil || strings.Contains(err.Error(), ref) {
			t.Fatalf("reference=%q err=%v", ref, err)
		}
	}
}

func TestFileCredentialSourceRejectsNoncanonicalRootWithoutLeakingIt(t *testing.T) {
	root, tokenPath := testCredentialRoot(t)
	if err := os.WriteFile(tokenPath, []byte("token"), 0o600); err != nil {
		t.Fatal(err)
	}
	unsafe := root + "/../" + filepath.Base(root)
	_, err := (FileCredentialSource{Root: unsafe}).Resolve(context.Background(), FileCredentialSourceRef)
	if err == nil || strings.Contains(err.Error(), unsafe) {
		t.Fatalf("error=%v", err)
	}
}

func testCredentialRoot(t *testing.T) (string, string) {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(base, "secrets")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	return root, filepath.Join(root, "linear-token")
}
