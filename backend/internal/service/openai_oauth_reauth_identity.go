package service

import (
	"encoding/base64"
	"encoding/json"
	"strings"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
)

type reauthTokenIdentity struct {
	account string
	user    string
	email   string
	sid     string
	sub     string
}

func decodeReauthTokenIdentity(token string) (reauthTokenIdentity, bool) {
	var identity reauthTokenIdentity
	if len(token) > 65536 {
		return identity, false
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return identity, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return identity, false
	}
	claims, ok := credentialOperationObject(payload)
	if !ok {
		return identity, false
	}
	claimString := func(fields map[string]json.RawMessage, key string) string {
		var value string
		_ = json.Unmarshal(fields[key], &value)
		return strings.TrimSpace(value)
	}
	identity.email = claimString(claims, "email")
	identity.sid = claimString(claims, "sid")
	identity.sub = claimString(claims, "sub")
	if raw, exists := claims["https://api.openai.com/auth"]; exists {
		auth, valid := credentialOperationObject(raw)
		if !valid {
			return identity, false
		}
		identity.account = claimString(auth, "chatgpt_account_id")
		identity.user = firstNonEmptyReauth(claimString(auth, "chatgpt_user_id"), claimString(auth, "user_id"))
	}
	if raw, exists := claims["https://api.openai.com/profile"]; exists {
		profile, valid := credentialOperationObject(raw)
		if !valid {
			return identity, false
		}
		email := claimString(profile, "email")
		if identity.email != "" && email != "" && !strings.EqualFold(identity.email, email) {
			return identity, false
		}
		identity.email = firstNonEmptyReauth(identity.email, email)
	}
	return identity, true
}

func resolveReauthTokenIdentity(info *OpenAITokenInfo) error {
	invalidClaims := func() error {
		return infraerrors.Conflict("OPENAI_REAUTH_IDENTITY_MISMATCH", "OAuth token identity claims are invalid or inconsistent")
	}
	if info == nil {
		return invalidClaims()
	}
	id, ok := decodeReauthTokenIdentity(info.IDToken)
	if !ok {
		return invalidClaims()
	}
	var access reauthTokenIdentity
	if strings.Contains(info.AccessToken, ".") {
		access, ok = decodeReauthTokenIdentity(info.AccessToken)
		if !ok {
			return invalidClaims()
		}
	}
	for _, pair := range [][2]string{
		{id.account, access.account}, {id.user, access.user},
		{strings.ToLower(id.email), strings.ToLower(access.email)},
	} {
		if pair[0] != "" && pair[1] != "" && pair[0] != pair[1] {
			return invalidClaims()
		}
	}
	info.ChatGPTAccountID = firstNonEmptyReauth(id.account, access.account, id.sid)
	info.ChatGPTUserID = firstNonEmptyReauth(id.user, access.user, id.sub)
	info.Email = firstNonEmptyReauth(id.email, access.email)
	return nil
}
