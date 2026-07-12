package proxy

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/synctv-org/synctv/internal/conf"
)

func TestM3u8TargetTokenIsOpaqueAndRecoverable(t *testing.T) {
	oldConf := conf.Conf
	conf.Conf = conf.DefaultConfig()
	conf.Conf.Jwt.Secret = "m3u8-test-secret"
	t.Cleanup(func() { conf.Conf = oldConf })
	clearM3u8TargetsForTest()

	const upstream = "https://upstream.example/video/playlist.m3u8?api_key=super-secret"
	token, err := NewM3u8TargetToken(upstream, "room-id", "movie-id", true)
	if err != nil {
		t.Fatalf("create target token: %v", err)
	}

	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("JWT parts = %d, want 3", len(parts))
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode JWT payload: %v", err)
	}
	for _, sensitive := range []string{"upstream.example", "api_key", "super-secret", upstream} {
		if strings.Contains(string(payload), sensitive) || strings.Contains(token, sensitive) {
			t.Fatalf("JWT leaked %q", sensitive)
		}
	}

	parsedToken, _, err := jwt.NewParser().ParseUnverified(token, &m3u8TargetClaims{})
	if err != nil {
		t.Fatalf("parse JWT claims: %v", err)
	}
	claims, ok := parsedToken.Claims.(*m3u8TargetClaims)
	if !ok || claims.ExpiresAt == nil || claims.TargetID == "" {
		t.Fatalf("JWT claims missing opaque target or expiry: %#v", parsedToken.Claims)
	}
	ttl := claims.ExpiresAt.Time.Sub(claims.NotBefore.Time)
	if ttl != m3u8TargetTTL {
		t.Fatalf("JWT TTL = %v, want %v", ttl, m3u8TargetTTL)
	}

	target, err := GetM3u8Target(token)
	if err != nil {
		t.Fatalf("recover target: %v", err)
	}
	if target.TargetURL != upstream || target.RoomID != "room-id" || target.MovieID != "movie-id" || !target.IsM3u8File {
		t.Fatalf("recovered target = %#v", target)
	}
}

func TestM3u8TargetBindingAndExpiryFailClosed(t *testing.T) {
	oldConf := conf.Conf
	conf.Conf = conf.DefaultConfig()
	conf.Conf.Jwt.Secret = "m3u8-test-secret"
	t.Cleanup(func() { conf.Conf = oldConf })
	clearM3u8TargetsForTest()

	expiresAt := time.Now().Add(time.Minute)
	targetID, err := storeM3u8Target("https://upstream.example/video.ts?api_key=secret", "room-id", "movie-id", expiresAt)
	if err != nil {
		t.Fatalf("store target: %v", err)
	}

	wrongBinding := signM3u8ClaimsForTest(t, targetID, "other-room", "movie-id", expiresAt)
	if _, err := GetM3u8Target(wrongBinding); err == nil {
		t.Fatal("wrong room binding was accepted")
	}

	expiredAt := time.Now().Add(-time.Minute)
	expiredID, err := storeM3u8Target("https://upstream.example/expired.ts?api_key=secret", "room-id", "movie-id", expiredAt)
	if err != nil {
		t.Fatalf("store expired target: %v", err)
	}
	expiredToken := signM3u8ClaimsForTest(t, expiredID, "room-id", "movie-id", expiredAt)
	if _, err := GetM3u8Target(expiredToken); err == nil {
		t.Fatal("expired target was accepted")
	}
}

func signM3u8ClaimsForTest(t *testing.T, targetID, roomID, movieID string, expiresAt time.Time) string {
	t.Helper()
	claims := &m3u8TargetClaims{
		RoomID: roomID,
		MovieID: movieID,
		TargetID: targetID,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(expiresAt),
		},
	}
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(conf.Conf.Jwt.Secret))
	if err != nil {
		t.Fatalf("sign target token: %v", err)
	}
	return token
}

func clearM3u8TargetsForTest() {
	m3u8Targets.Lock()
	defer m3u8Targets.Unlock()
	m3u8Targets.items = make(map[string]storedM3u8Target)
}
