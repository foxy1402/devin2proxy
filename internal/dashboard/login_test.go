package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The login loop: a remote browser over plain HTTP signs in, the server logs
// "signed in", and the page comes straight back to the password form. The
// cookie is the culprit — to a browser, http://127.0.0.1 is a trustworthy
// origin and an insecure cookie is stored without comment, while http://<ip>
// is not, and privacy-configured browsers refuse it outright. Go's cookie
// parser and httptest are lenient where Chrome is not, which is why the whole
// existing suite passes and a real deployment loops.
//
// The fix is a second rail: the login response carries the session value as
// JSON, and the page sends it as an Authorization: Bearer header on every
// call afterwards. These tests pin both rails and the page wiring, because
// every one of the three faults below looks like "working" in a local test.

func TestALoginResponseCarriesTheSessionValueAsJSON(t *testing.T) {
	d := testDashboard(t)
	rec := post(t, d, "/dashboard/api/login", map[string]string{"password": "correct-horse-battery"})
	if rec.Code != http.StatusOK {
		t.Fatalf("login: status %d, body %s", rec.Code, rec.Body)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("login response is not JSON: %v (%s)", err, rec.Body)
	}
	token, _ := body["token"].(string)
	if token == "" {
		t.Fatal("the login response carries no token; a browser that refuses the insecure cookie has no second rail")
	}
	if body["authenticated"] != true {
		t.Errorf("the login response does not say authenticated: %v", body)
	}
}

func TestABearerTokenAuthenticatesWithoutAnyCookie(t *testing.T) {
	d := testDashboard(t)
	rec := post(t, d, "/dashboard/api/login", map[string]string{"password": "correct-horse-battery"})
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("login response is not JSON: %v", err)
	}
	token, _ := body["token"].(string)
	if token == "" {
		t.Fatal("no token in the login response")
	}

	req := httptest.NewRequest(http.MethodGet, "/dashboard/api/accounts", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	d.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("bearer-only request: status %d, want 200 (body %s)", rr.Code, rr.Body)
	}

	// The cookie rail still works on its own, so a browser that keeps the
	// cookie is unaffected by any of this.
	if got := get(t, d, "/dashboard/api/accounts", sessionCookieFrom(t, rec)); got.Code != http.StatusOK {
		t.Errorf("cookie-only request: status %d, want 200", got.Code)
	}
}

func TestAForgedBearerTokenIsRefused(t *testing.T) {
	d := testDashboard(t)
	req := httptest.NewRequest(http.MethodGet, "/dashboard/api/accounts", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	req.Header.Set("Authorization", "Bearer not-a-real-session")
	rr := httptest.NewRecorder()
	d.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("forged bearer: status %d, want 401", rr.Code)
	}
}

func TestThePageSendsTheTokenItWasGiven(t *testing.T) {
	d := testDashboard(t)
	page := get(t, d, "/dashboard/").Body.String()

	// The login handler stores the token from the response...
	if !strings.Contains(page, "setToken(data.token)") {
		t.Error("signIn does not store the token the login response carries")
	}
	// ...every api() call sends it as a bearer header...
	if !strings.Contains(page, `init.headers["Authorization"] = "Bearer " + S.token`) {
		t.Error("api() does not send the stored token as an Authorization header")
	}
	// ...a 401 drops it, so a stale token cannot outlive its epoch...
	if !strings.Contains(page, "if (res.status === 401) setToken(null)") {
		t.Error("api() does not clear the stored token on a 401")
	}
	// ...and signing out drops it too.
	signOut := page[strings.Index(page, "async function signOut()"):]
	if end := strings.Index(signOut[1:], "\n  async function "); end >= 0 {
		signOut = signOut[:end+1]
	}
	if !strings.Contains(signOut, "setToken(null)") {
		t.Error("signOut does not clear the stored token")
	}
}

func TestTheSignInButtonIsNotATypelessSubmitButton(t *testing.T) {
	// A typeless <button> inside a <form> is a submit button, so Enter in the
	// password field fired both the keydown handler and the form submit: two
	// logins per keystroke, two "signed in" log lines, and a re-render that
	// read as a lost session. The fix is an explicit type="button"; this pins
	// it because the browser raises no error for the broken shape.
	d := testDashboard(t)
	page := get(t, d, "/dashboard/").Body.String()
	render := page[strings.Index(page, "function renderLogin()"):]
	if end := strings.Index(render[1:], "\n  function "); end >= 0 {
		render = render[:end+1]
	}
	if !strings.Contains(render, `el("button", { type: "button"`) {
		t.Error("the Sign in button has no explicit type; inside the form it submits, double-firing the login")
	}
}
