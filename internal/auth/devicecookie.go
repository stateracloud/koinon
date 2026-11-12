// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/stateracloud/koinon/internal/auth/ratelimit"
)

// The device cookie slows down guesses at an account without locking its owner out (OWASP, "Slow Down
// Online Guessing Attacks with Device Cookies"). A successful password login sets it: the lowercased
// username, the time it was issued and a random nonce, signed with HMAC-SHA256 under a key derived
// from the install mark (Config.DeviceCookieSecret). A login that carries a cookie that verifies,
// belongs to the username being tried and is under 30 days old skips the account level of the limiter
// (loginlimit.go); the key, address and group levels still apply. Failures with such a cookie count
// against the cookie, not the account, and a cookie that fails deviceFreeFails times in a window is
// refused by the limiter like any key and so loses the exemption while its own block lasts. A new
// login sets a new cookie with a new nonce. The cookie is HttpOnly, Secure (unless the public URL is
// plain http) and SameSite=Strict, and it holds no secret: a stolen cookie lets a thief try
// deviceFreeFails guesses per window at that one account, as the owner could.
//
// The cookie bypasses the account level only. The key, address and group levels still apply
// (BlockedTrusted); the overflow refusal for untracked sources does not.
//
// The signing key cannot rotate today: it is derived from the install mark, and a changed mark makes
// every cookie fail to verify (the owner then waits like anyone else until the next login). The
// payload starts with deviceCookieVersion so that a dedicated Secret or a rotation can be added later
// without breaking the parsing of cookies in the field.
const (
	deviceCookieVersion = "v1"

	deviceCookieLifetime = 30 * 24 * time.Hour
	deviceFreeFails      = 10

	deviceCookieName         = "__Host-koinon_device"
	insecureDeviceCookieName = "koinon_device"
)

func (s *Service) deviceCookieName() string {
	if s.secureCookies {
		return deviceCookieName
	}
	return insecureDeviceCookieName
}

// deriveDeviceKey is the signing key for device cookies, bound to its purpose. Nil when there is no
// secret: no device cookie is issued then, and none is honoured.
func deriveDeviceKey(secret []byte) []byte {
	if len(secret) == 0 {
		return nil
	}
	m := hmac.New(sha256.New, secret)
	m.Write([]byte("koinon/login-device-cookie/v1"))
	return m.Sum(nil)
}

func (s *Service) deviceMAC(payload string) string {
	m := hmac.New(sha256.New, s.deviceKey)
	m.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// deviceLimiterKey is the limiter key of a cookie's failures.
func deviceLimiterKey(nonce string) string { return ratelimit.Key("device", nonce) }

// deviceNonce returns the nonce of the request's device cookie when it verifies for username and is
// not expired, else "".
func (s *Service) deviceNonce(r *http.Request, username string, now time.Time) string {
	if s.deviceKey == nil {
		return ""
	}
	c, err := r.Cookie(s.deviceCookieName())
	if err != nil {
		return ""
	}
	i := strings.LastIndexByte(c.Value, '.')
	if i < 0 {
		return ""
	}
	payload, mac := c.Value[:i], c.Value[i+1:]
	if !hmac.Equal([]byte(mac), []byte(s.deviceMAC(payload))) {
		return ""
	}
	parts := strings.Split(payload, ".")
	if len(parts) != 4 || parts[0] != deviceCookieVersion {
		return ""
	}
	parts = parts[1:]
	name, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil || string(name) != deviceUsername(username) {
		return ""
	}
	issued, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return ""
	}
	if age := now.Sub(time.Unix(issued, 0)); age < -time.Minute || age > deviceCookieLifetime {
		return ""
	}
	return parts[2]
}

func deviceUsername(username string) string { return strings.ToLower(auditedUsername(username)) }

// setDeviceCookie issues a device cookie for username, with a new nonce.
func (s *Service) setDeviceCookie(w http.ResponseWriter, username string, now time.Time) {
	if s.deviceKey == nil {
		return
	}
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return
	}
	payload := deviceCookieVersion + "." + base64.RawURLEncoding.EncodeToString([]byte(deviceUsername(username))) + "." +
		strconv.FormatInt(now.Unix(), 10) + "." + base64.RawURLEncoding.EncodeToString(nonce)
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // Secure is off only for a plain http:// public URL (Config.PublicURL)
		Name:     s.deviceCookieName(),
		Value:    payload + "." + s.deviceMAC(payload),
		Path:     "/",
		MaxAge:   int(deviceCookieLifetime / time.Second),
		HttpOnly: true,
		Secure:   s.secureCookies,
		SameSite: http.SameSiteStrictMode,
	})
}
