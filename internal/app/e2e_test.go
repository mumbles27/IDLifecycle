package app

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"idlifecycle/internal/ad"
	"idlifecycle/internal/api"
	"idlifecycle/internal/engine"
	"idlifecycle/internal/model"
	"idlifecycle/internal/secrets"
	"idlifecycle/internal/store/storetest"
	"idlifecycle/internal/web"
)

func findEdge() string {
	for _, p := range []string{
		`C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe`,
		`C:\Program Files\Microsoft\Edge\Application\msedge.exe`,
		`C:\Program Files\Google\Chrome\Application\chrome.exe`,
	} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// TestGUIEndToEndInHeadlessBrowser drives the real UI (served with the production CSP) against the real
// API and a fake directory. It is skipped when no Chromium-based browser is installed.
func TestGUIEndToEndInHeadlessBrowser(t *testing.T) {
	browser := findEdge()
	if browser == "" {
		t.Skip("no Edge/Chrome installed")
	}
	api.BcryptCost = 4
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	st := storetest.New(t)
	cipher, _ := secrets.New(make([]byte, 32))
	fake := ad.NewFake()
	fake.AddGroup("GG-All", "CN=GG-All,OU=Groups,DC=corp,DC=test")
	eng := engine.New(st, fake, cipher, log)
	ctx := context.Background()

	// admin + live settings + seed data producing pending changes
	h, _ := api.HashPassword("e2e-admin-password")
	_ = st.CreateUser(ctx, model.User{ID: uuid.NewString(), Username: "admin", PasswordHash: h, Role: model.RoleAdmin, CreatedAt: time.Now()})
	s := model.DefaultSettings()
	s.Engine.DryRun = false
	s.AD.SearchBases = []string{"OU=Staff,DC=corp,DC=test"}
	s.AD.DefaultOU = "OU=Staff,DC=corp,DC=test"
	s.AD.UPNSuffix = "corp.test"
	s.Correlate.Enabled = true // default rules: first + last name required
	_ = st.SaveSettings(ctx, s, "e2e")

	// Fixtures for the identity scenarios: a taken username (Carl Name -> cname), a person with two possible accounts
	// (Maria Lopez) and an account nobody matches (kiosk01).
	fake.AddUser(ad.FakeUser{DN: "CN=Carl Other,OU=Elsewhere,DC=corp,DC=test", Sam: "cname", UPN: "carl.other@corp.test", Enabled: true})
	for _, sam := range []string{"mlopez", "mlopez2"} {
		fake.AddUser(ad.FakeUser{DN: "CN=Maria Lopez " + sam + ",OU=Staff,DC=corp,DC=test", Sam: sam, UPN: sam + "@corp.test", Enabled: true,
			Attrs: map[string]string{"givenName": "Maria", "sn": "Lopez"}})
	}
	fake.AddUser(ad.FakeUser{DN: "CN=kiosk01,OU=Staff,DC=corp,DC=test", Sam: "kiosk01", UPN: "kiosk01@corp.test", Enabled: true})

	dir := t.TempDir()
	seedCSV := filepath.Join(dir, "seed.csv")
	_ = os.WriteFile(seedCSV, []byte("EmployeeID,FirstName,LastName\nE1,Jane,Doe\nE2,Bob,Smith\nE3,Carl,Name\nE5,Maria,Lopez\n"), 0o600)
	_ = st.CreateSource(ctx, model.Source{ID: "seed", Name: "seed", Type: "csv", Enabled: true, CreatedAt: time.Now(), UpdatedAt: time.Now(),
		Options: model.SourceOptions{ExternalIDField: "EmployeeID"}, Config: map[string]any{"path": seedCSV}})
	if run, err := eng.RunNow(ctx, "e2e", "e2e"); err != nil || run.Counters["pending"] != 4 {
		t.Fatalf("seed run: %v %+v", err, run)
	}
	uiCSV := filepath.Join(dir, "selftest.csv")
	_ = os.WriteFile(uiCSV, []byte("EmployeeID,FirstName,LastName\nX1,Test,User\n"), 0o600)

	// Static = the product UI plus the self-test pages and a report sink; all of it behind the API's CSP middleware.
	reports := make(chan string, 1)
	selftest := http.StripPrefix("/selftest/", http.FileServer(http.Dir(filepath.Join("..", "web", "testdata", "selftest"))))
	static := http.NewServeMux()
	static.Handle("/selftest/", selftest)
	static.HandleFunc("POST /__report", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		select {
		case reports <- string(b):
		default:
		}
		w.WriteHeader(http.StatusNoContent)
	})
	static.Handle("/", web.Handler())

	srv := httptest.NewServer((&api.Server{Store: st, Engine: eng, Cipher: cipher, Static: static, Log: log, Version: "e2e", BaseCtx: ctx}).Handler())
	defer srv.Close()

	q := url.Values{"user": {"admin"}, "pass": {"e2e-admin-password"}, "csvpath": {uiCSV}, "report": {"/__report"}}
	target := srv.URL + "/selftest/selftest.html?" + q.Encode()

	profile, err := os.MkdirTemp("", "idl-e2e-profile-")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(browser, "--headless=new", "--disable-gpu", "--no-first-run", "--no-default-browser-check",
		"--disable-extensions", "--user-data-dir="+profile, "--window-size=1400,1000", target)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = exec.Command("taskkill", "/T", "/F", "/PID", itoa(cmd.Process.Pid)).Run()
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
		time.Sleep(500 * time.Millisecond)
		_ = os.RemoveAll(profile) // best effort: the browser can hold files briefly after exit
	}()

	select {
	case rep := <-reports:
		if !strings.HasPrefix(strings.TrimSpace(rep), "PASS") {
			t.Fatalf("browser self-test failed:\n%s", rep)
		}
		t.Logf("browser self-test:\n%s", rep)
	case <-time.After(3 * time.Minute):
		t.Fatal("browser self-test did not report within 3 minutes")
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
