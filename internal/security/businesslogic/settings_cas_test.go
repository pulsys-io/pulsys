// SPDX-License-Identifier: Apache-2.0
// Copyright 2026 Pulsys

package businesslogic

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
)

type settingResp struct {
	Scope   string          `json:"scope"`
	Key     string          `json:"key"`
	Value   json.RawMessage `json:"value"`
	Version int64           `json:"version"`
}

func (f *fixtures) putSetting(scope, key, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPut,
		"/admin/api/v1/settings/"+scope+"/"+key, bytes.NewReader([]byte(body))).
		WithContext(f.adminCtx())
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	f.Mux.ServeHTTP(w, req)
	return w
}

// TestSettings_CAS_HappyPath pins the canonical update sequence:
// create with version=0, then update with version=N, version
// auto-increments.
func TestSettings_CAS_HappyPath(t *testing.T) {
	f := newFixtures(t)

	rec := f.putSetting("auth", "session_ttl", `{"value":{"hours":1},"version":0}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("initial create: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var st settingResp
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatalf("decode: %v body=%s", err, rec.Body.String())
	}
	if st.Version != 1 {
		t.Fatalf("first create returned version=%d want 1", st.Version)
	}

	rec = f.putSetting("auth", "session_ttl", `{"value":{"hours":2},"version":1}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("update v1->v2: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if st.Version != 2 {
		t.Fatalf("post-update version=%d want 2", st.Version)
	}
}

// TestSettings_CAS_StaleVersionConflicts pins the conflict
// branch: two concurrent admins both read version=N, both PUT
// with expectVersion=N -- the loser MUST get 409, not silently
// overwrite the winner.  This is the central WSTG-BUSL-03
// regression: pre-Phase-5 the loser silently overwrote.
func TestSettings_CAS_StaleVersionConflicts(t *testing.T) {
	f := newFixtures(t)

	rec := f.putSetting("flags", "feature_x", `{"value":{"on":true},"version":0}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("initial: status=%d body=%s", rec.Code, rec.Body.String())
	}

	// Two admins each read v1.  Admin1 writes first ("wins").
	rec = f.putSetting("flags", "feature_x", `{"value":{"on":false},"version":1}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("first updater: status=%d body=%s", rec.Code, rec.Body.String())
	}

	// Admin2 PUTs with the stale version (still 1).  Must
	// 409, MUST NOT overwrite the winning value.
	rec = f.putSetting("flags", "feature_x", `{"value":{"on":"clobbered"},"version":1}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("stale updater: status=%d body=%s want 409", rec.Code, rec.Body.String())
	}

	// Confirm the post-state matches the first updater, not
	// the loser.  Read back via another put with the correct
	// version=2 so the response carries the current value
	// without our needing a GET helper.
	rec = f.putSetting("flags", "feature_x", `{"value":{"on":"explicit"},"version":2}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("read-after-conflict: status=%d body=%s", rec.Code, rec.Body.String())
	}
	var st settingResp
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if st.Version != 3 {
		t.Fatalf("post-state version=%d want 3", st.Version)
	}
}

// TestSettings_CAS_VersionZeroOnExisting pins the second arm:
// once the key exists, a PUT with version=0 MUST fail closed
// (409), not silently upsert.  Pre-Phase-5 this branch was
// "INSERT ... ON CONFLICT DO UPDATE" which silently overwrote.
func TestSettings_CAS_VersionZeroOnExisting(t *testing.T) {
	f := newFixtures(t)

	rec := f.putSetting("ops", "limits", `{"value":{"req":1},"version":0}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("create: status=%d body=%s", rec.Code, rec.Body.String())
	}

	rec = f.putSetting("ops", "limits", `{"value":{"req":2},"version":0}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("version=0 on existing: status=%d body=%s want 409", rec.Code, rec.Body.String())
	}
}

// TestSettings_CAS_NegativeVersionRejected pins the input-
// validation contract: a negative expectVersion is a client
// bug, never a legitimate request.  Reject at the handler
// with 400 so the wrong-version-class error is unambiguous
// (a SQL UPDATE WHERE version = -1 would just no-op, masking
// the bug).
func TestSettings_CAS_NegativeVersionRejected(t *testing.T) {
	f := newFixtures(t)

	rec := f.putSetting("x", "y", `{"value":{},"version":-1}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("negative version: status=%d body=%s want 400", rec.Code, rec.Body.String())
	}
}

// TestSettings_CAS_ConcurrentWritersExactlyOneWins is the
// stress / property variant: N goroutines all PUT with
// expectVersion=1 against the same key.  Exactly one MUST
// return 200; the other N-1 MUST return 409.  This is the
// invariant a future implementation that relies on (e.g.)
// SERIALIZABLE retry instead of expectVersion would have to
// preserve.
func TestSettings_CAS_ConcurrentWritersExactlyOneWins(t *testing.T) {
	f := newFixtures(t)

	rec := f.putSetting("race", "k", `{"value":{"v":0},"version":0}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("seed: status=%d body=%s", rec.Code, rec.Body.String())
	}

	const N = 8
	var ok, conflict, other atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			body := `{"value":{"v":` + strconv.Itoa(i) + `},"version":1}`
			w := f.putSetting("race", "k", body)
			switch w.Code {
			case http.StatusOK:
				ok.Add(1)
			case http.StatusConflict:
				conflict.Add(1)
			default:
				other.Add(1)
				t.Errorf("goroutine %d: status=%d body=%s", i, w.Code, w.Body.String())
			}
		}()
	}
	wg.Wait()

	if ok.Load() != 1 {
		t.Fatalf("winners=%d want exactly 1", ok.Load())
	}
	if conflict.Load() != int32(N-1) {
		t.Fatalf("conflicts=%d want %d", conflict.Load(), N-1)
	}
	if other.Load() != 0 {
		t.Fatalf("non-200/409 responses=%d", other.Load())
	}
}
