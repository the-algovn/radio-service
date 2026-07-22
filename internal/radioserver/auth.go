package radioserver

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"google.golang.org/grpc/metadata"
)

// identityFromContext does the single read-only segment-2 decode of the
// forwarded, gateway-verified JWT per authnz-conventions.md — the gateway
// already verified the signature and is the sole verified ingress. Returns
// the subject plus a server-derived display name (name →
// preferred_username → "thính giả <sub6>"). The SPA never supplies the name.
func identityFromContext(ctx context.Context) (sub, displayName string, err error) {
	md, _ := metadata.FromIncomingContext(ctx)
	vals := md.Get("authorization")
	if len(vals) == 0 {
		return "", "", errors.New("no authorization metadata")
	}
	parts := strings.Split(strings.TrimPrefix(vals[0], "Bearer "), ".")
	if len(parts) != 3 {
		return "", "", errors.New("not a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", "", errors.New("bad JWT payload")
	}
	var claims struct {
		Sub               string `json:"sub"`
		Name              string `json:"name"`
		PreferredUsername string `json:"preferred_username"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil || claims.Sub == "" {
		return "", "", errors.New("bad claims")
	}
	name := claims.Name
	if name == "" {
		name = claims.PreferredUsername
	}
	if name == "" {
		name = fmt.Sprintf("thính giả %.6s", claims.Sub)
	}
	return claims.Sub, name, nil
}
