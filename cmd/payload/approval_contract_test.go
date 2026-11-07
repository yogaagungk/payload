package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/yogaagungk/payload/internal/service"
)

func contractFixture(t *testing.T, name string) []byte {
	t.Helper()
	// #nosec G304 -- fixture path is test-controlled.
	data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "contract", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return data
}

func TestApprovalContractFixtures(t *testing.T) {
	var request service.ReleaseApprovalRequest
	if err := json.Unmarshal(contractFixture(t, "create_request.json"), &request); err != nil {
		t.Fatal(err)
	}
	if request.Repo != "yogaagungk/my-app" || request.SHA != "0123456789abcdef0123456789abcdef01234567" {
		t.Errorf("request = %+v", request)
	}
	if request.Inputs["image_tag"] != "v1.2.3" {
		t.Errorf("inputs = %v", request.Inputs)
	}

	cases := map[string]int{
		"release_pending_201.json":     0,
		"release_pending_200.json":     0,
		"release_approved.json":        0,
		"release_dispatched_run.json":  0,
		"release_dispatched_null.json": 0,
		"release_rejected.json":        3,
		"release_expired.json":         4,
		"release_dispatch_failed.json": 5,
	}
	for name, wantCode := range cases {
		var release service.HoustonRelease
		if err := json.Unmarshal(contractFixture(t, name), &release); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		code, _, terminal := terminalRelease(release)
		if wantCode == 0 {
			if terminal {
				t.Errorf("%s unexpectedly terminal with code %d", name, code)
			}
			continue
		}
		if !terminal || code != wantCode {
			t.Errorf("%s = code %d terminal=%v, want %d", name, code, terminal, wantCode)
		}
	}
	if release := func() service.HoustonRelease {
		var value service.HoustonRelease
		_ = json.Unmarshal(contractFixture(t, "release_dispatched_null.json"), &value)
		return value
	}(); release.RunID != nil {
		t.Errorf("null run fixture decodes to %v", *release.RunID)
	}
}

func TestApprovalContractErrorEnvelopes(t *testing.T) {
	for name, want := range map[string]string{"error_401.json": "unauthorized", "error_422.json": "validation_failed"} {
		var body struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(contractFixture(t, name), &body); err != nil {
			t.Fatal(err)
		}
		if body.Error.Code != want {
			t.Errorf("%s code = %q, want %q", name, body.Error.Code, want)
		}
	}
}
