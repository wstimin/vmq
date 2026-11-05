package maintenance

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

type testTransport func(*http.Request) (*http.Response, error)

func (f testTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func response(data []byte) *http.Response {
	return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(data)), Header: make(http.Header)}
}

func TestVersionOrderingAndRepositoryValidation(t *testing.T) {
	for _, test := range []struct {
		a, b string
		want bool
	}{
		{"v3.1.0-web.2", "v3.1.0-web.1", true}, {"v3.1.0", "v3.1.0-web.2", true}, {"v3.1.1", "v3.1.0", true},
		{"v3.1.0-web.2", "v3.1.0", false}, {"v3.0.9", "v3.1.0", false}, {"v3.1.0", "v3.1.0", false}, {"latest", "v3.1.0", false},
		{"v999999999999999.0.0", "v3.1.0", false},
	} {
		if got := newer(test.a, test.b); got != test.want {
			t.Fatalf("newer(%s,%s)=%v", test.a, test.b, got)
		}
	}
	for _, input := range []string{"wstimin/vmq", "https://github.com/wstimin/vmq/", "https://github.com/wstimin/vmq.git"} {
		if got, err := normalizeRepository(input); err != nil || got != "wstimin/vmq" {
			t.Fatalf("repo %q: %s %v", input, got, err)
		}
	}
	for _, input := range []string{"https://evil.example/wstimin/vmq", "http://github.com/wstimin/vmq", "wstimin/vmq/extra", "../vmq", "wstimin/vmq?token=abc"} {
		if _, err := normalizeRepository(input); err == nil {
			t.Fatalf("unsafe repository accepted: %s", input)
		}
	}
}

func TestGithubReleaseRequiresMatchingBinaryAndChecksum(t *testing.T) {
	for _, scenario := range []string{"digest", "checksum file", "missing checksum", "wrong arch", "prerelease", "network error"} {
		t.Run(scenario, func(t *testing.T) {
			u, err := NewUpdater(t.TempDir(), "unused", nil)
			if err != nil {
				t.Fatal(err)
			}
			digest := strings.Repeat("a", 64)
			asset := releaseAsset{ID: 1, Name: "vmqfox-api-linux-" + runtime.GOARCH, Size: 100, Digest: "sha256:" + digest}
			if scenario == "missing checksum" || scenario == "checksum file" {
				asset.Digest = ""
			}
			if scenario == "wrong arch" {
				asset.Name = "wrong-binary"
			}
			rel := release{Tag: "v3.1.0-web.2", Prerelease: scenario == "prerelease", Assets: []releaseAsset{asset}}
			if scenario == "checksum file" {
				rel.Assets = append(rel.Assets, releaseAsset{ID: 2, Name: "checksums.txt", Size: 100})
			}
			u.config.Token = "private-test-token"
			u.client = &http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
				if r.URL.Host != "api.github.com" || r.Header.Get("Authorization") != "Bearer private-test-token" {
					t.Fatal("wrong API or auth header")
				}
				if scenario == "network error" {
					return &http.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader("secret-error"))}, nil
				}
				if strings.HasSuffix(r.URL.Path, "/assets/2") {
					return response([]byte(digest + "  " + asset.Name + "\n")), nil
				}
				data, _ := json.Marshal(rel)
				return response(data), nil
			})}
			_, tag, got, err := u.latest(context.Background())
			if scenario == "digest" || scenario == "checksum file" {
				if err != nil || tag != rel.Tag || got != digest {
					t.Fatalf("release: %s %s %v", tag, got, err)
				}
			} else if err == nil {
				t.Fatal("invalid release accepted")
			}
			data, _ := json.Marshal(u.Info())
			if strings.Contains(string(data), u.config.Token) {
				t.Fatal("token exposed to browser")
			}
		})
	}
}

func elfFixture() []byte {
	data := make([]byte, 64)
	copy(data, "\x7fELF")
	data[4], data[5], data[6] = 2, 1, 1
	binary.LittleEndian.PutUint16(data[16:18], 2)
	machine := uint16(62)
	if runtime.GOARCH == "arm64" {
		machine = 183
	}
	binary.LittleEndian.PutUint16(data[18:20], machine)
	binary.LittleEndian.PutUint32(data[20:24], 1)
	binary.LittleEndian.PutUint16(data[52:54], 64)
	return data
}

func TestDownloadVerifiesBeforeReplacingAndPreservesBackup(t *testing.T) {
	for _, scenario := range []string{"valid", "bad checksum", "truncated", "extra bytes", "wrong architecture", "HTML"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			exe := filepath.Join(dir, "vmqfox-api-linux-"+runtime.GOARCH)
			if err := os.WriteFile(exe, []byte("old program"), 0755); err != nil {
				t.Fatal(err)
			}
			u, err := NewUpdater(dir, exe, nil)
			if err != nil {
				t.Fatal(err)
			}
			data := elfFixture()
			size := int64(len(data))
			sum := sha256.Sum256(data)
			digest := hex.EncodeToString(sum[:])
			switch scenario {
			case "bad checksum":
				digest = strings.Repeat("0", 64)
			case "truncated":
				data = data[:len(data)-1]
			case "extra bytes":
				data = append(data, 1)
			case "wrong architecture":
				data[18], data[19] = 0, 0
				sum = sha256.Sum256(data)
				digest = hex.EncodeToString(sum[:])
			case "HTML":
				copy(data, []byte("<html>404</html>"))
				sum = sha256.Sum256(data)
				digest = hex.EncodeToString(sum[:])
			}
			u.client = &http.Client{Transport: testTransport(func(*http.Request) (*http.Response, error) { return response(data), nil })}
			err = u.downloadAndReplace(context.Background(), releaseAsset{ID: 1, Size: size}, "v3.1.0-web.2", digest)
			current, _ := os.ReadFile(exe)
			if scenario == "valid" {
				if err != nil || !bytes.Equal(current, data) {
					t.Fatal("replacement failed", err)
				}
				backup, _ := os.ReadFile(exe + ".previous")
				if string(backup) != "old program" {
					t.Fatal("original backup missing")
				}
			} else if err == nil || string(current) != "old program" {
				t.Fatalf("invalid binary replaced original: %v", err)
			}
			matches, _ := filepath.Glob(filepath.Join(dir, ".vmq-update-*"))
			if len(matches) != 0 {
				t.Fatal("partial download left behind")
			}
		})
	}
}

func TestUpdateFirstBootConfirmationAndRollback(t *testing.T) {
	for _, healthy := range []bool{true, false} {
		dir := t.TempDir()
		exe := filepath.Join(dir, "program")
		_ = os.WriteFile(exe, []byte("new"), 0755)
		_ = os.WriteFile(exe+".previous", []byte("old"), 0755)
		state := UpdateState{Phase: "restarting", LatestVersion: Version, PreviousVersion: "v3.0.0"}
		data, _ := json.Marshal(state)
		if err := writePrivate(filepath.Join(dir, ".update-state.json"), data); err != nil {
			t.Fatal(err)
		}
		rolled, err := RecoverUpdate(dir, exe)
		if err != nil || rolled {
			t.Fatal("first boot should try new version", err)
		}
		if healthy {
			if err := MarkUpdateHealthy(dir); err != nil {
				t.Fatal(err)
			}
			rolled, err = RecoverUpdate(dir, exe)
			if err != nil || rolled {
				t.Fatal("healthy version rolled back")
			}
			u, err := NewUpdater(dir, exe, nil)
			if err != nil || u.Info().Phase != "completed" {
				t.Fatal("completion not persisted")
			}
		} else {
			rolled, err = RecoverUpdate(dir, exe)
			if err != nil || !rolled {
				t.Fatal("failed version did not roll back", err)
			}
			data, _ := os.ReadFile(exe)
			if string(data) != "old" {
				t.Fatal("wrong restored binary")
			}
		}
	}
}

func TestSourceTokenSavedPrivatelyAndUnmanagedUpdateRejected(t *testing.T) {
	dir := t.TempDir()
	u, err := NewUpdater(dir, "unused", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := u.SaveSource("wstimin/vmq", "github-test-secret"); err != nil {
		t.Fatal(err)
	}
	if err := u.SaveSource("wstimin/vmq", ""); err != nil {
		t.Fatal(err)
	}
	u2, err := NewUpdater(dir, "unused", nil)
	if err != nil || u2.config.Token != "github-test-secret" {
		t.Fatal("saved token lost")
	}
	if err := u2.Start(context.Background()); err == nil {
		t.Fatal("unsupervised program allowed self replacement")
	}
	info, _ := json.Marshal(u2.Info())
	if strings.Contains(string(info), "github-test-secret") {
		t.Fatal("token leaked")
	}
}
