package configloader_test

import (
	"os"
	"path/filepath"
	"testing"

	config "github.com/gtkit/gsrv/internal/pkg/configloader"
)

func TestStrictLoad(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		ok         bool
	}{
		{"valid", "name: demo\n", true}, {"unknown", "other: demo\n", false}, {"multiple", "name: demo\n---\nname: next\n", false}, {"empty", "", false}, {"duplicate", "name: a\nname: b\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "config.yml")
			if err := os.WriteFile(file, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			var out struct {
				Name string `yaml:"name"`
			}
			err := config.Load(file, &out)
			if (err == nil) != tc.ok {
				t.Fatalf("ok=%v err=%v", tc.ok, err)
			}
		})
	}
	if err := config.Load("missing", nil); err == nil {
		t.Fatal("nil accepted")
	}
}
func TestSecret(t *testing.T) {
	t.Setenv("GSRV_TEST_SECRET", "value")
	if value, err := config.Secret("GSRV_TEST_SECRET"); err != nil || value != "value" {
		t.Fatal("secret load failed")
	}
	t.Setenv("GSRV_TEST_SECRET", "")
	if _, err := config.Secret("GSRV_TEST_SECRET"); err == nil {
		t.Fatal("empty accepted")
	}
}
