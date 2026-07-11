package auth

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func TestAcquireDesignerTokenReacquiresExpiredBrokerToken(t *testing.T) {
	useTemporaryWorkingDirectory(t)
	tm := NewTokenManager("tenant", "client", "scope", "refresh", "cache")
	if err := tm.writeBrokerRefreshToken("expired-token"); err != nil {
		t.Fatalf("write expired broker refresh token: %v", err)
	}

	var requests []string
	tm.designerTokenRequest = func(refreshToken string) (string, int, error) {
		requests = append(requests, refreshToken)
		if refreshToken == "expired-token" {
			return "", 0, &designerOAuthError{
				Status:      http.StatusBadRequest,
				Code:        "invalid_grant",
				Description: "AADSTS700084: refresh token expired",
			}
		}
		return "designer-token", 3600, nil
	}

	acquisitions := 0
	tm.brokerTokenAcquisition = func() (string, error) {
		acquisitions++
		return "replacement-token", nil
	}

	token, expiresIn, err := tm.acquireDesignerToken()
	if err != nil {
		t.Fatalf("acquire designer token: %v", err)
	}
	if token != "designer-token" || expiresIn != 3600 {
		t.Fatalf("unexpected token result: token=%q expiresIn=%d", token, expiresIn)
	}
	if acquisitions != 1 {
		t.Fatalf("expected one SSO acquisition, got %d", acquisitions)
	}
	if len(requests) != 2 || requests[0] != "expired-token" || requests[1] != "replacement-token" {
		t.Fatalf("unexpected refresh token requests: %v", requests)
	}
}

func TestAcquireDesignerTokenDoesNotReacquireTransientFailure(t *testing.T) {
	useTemporaryWorkingDirectory(t)
	tm := NewTokenManager("tenant", "client", "scope", "refresh", "cache")
	if err := tm.writeBrokerRefreshToken("existing-token"); err != nil {
		t.Fatalf("write broker refresh token: %v", err)
	}

	transientErr := &designerOAuthError{
		Status:      http.StatusInternalServerError,
		Code:        "temporarily_unavailable",
		Description: "retry later",
	}
	tm.designerTokenRequest = func(string) (string, int, error) {
		return "", 0, transientErr
	}
	tm.brokerTokenAcquisition = func() (string, error) {
		t.Fatal("SSO acquisition must not run for transient failures")
		return "", nil
	}

	_, _, err := tm.acquireDesignerToken()
	if !errors.Is(err, transientErr) {
		t.Fatalf("expected transient error, got %v", err)
	}
}

func useTemporaryWorkingDirectory(t *testing.T) {
	t.Helper()
	originalDirectory, err := os.Getwd()
	if err != nil {
		t.Fatalf("get working directory: %v", err)
	}
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatalf("change working directory: %v", err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(originalDirectory); err != nil {
			t.Errorf("restore working directory: %v", err)
		}
	})

	if err := os.MkdirAll(filepath.Dir(designerBrokerRefreshFile), 0700); err != nil {
		t.Fatalf("create token directory: %v", err)
	}
}
