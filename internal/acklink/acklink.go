// Package acklink signs and verifies one-click acknowledge links. A link is
// <public_url>/ack/<incident id>.<hmac>, so only vigil can mint one and it
// works for exactly one incident.
package acklink

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"strconv"
	"strings"
)

func mac(secret string, id int64) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte("vigil-ack-v1:" + strconv.FormatInt(id, 10)))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil)[:18])
}

// URL returns the ack link, or "" when links are not configured.
func URL(publicURL, secret string, id int64) string {
	if publicURL == "" || secret == "" || id <= 0 {
		return ""
	}
	return strings.TrimRight(publicURL, "/") + "/ack/" + strconv.FormatInt(id, 10) + "." + mac(secret, id)
}

// Verify returns the incident ID of a valid token.
func Verify(secret, token string) (int64, bool) {
	idStr, sig, ok := strings.Cut(token, ".")
	if !ok || secret == "" {
		return 0, false
	}
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, hmac.Equal([]byte(sig), []byte(mac(secret, id)))
}
