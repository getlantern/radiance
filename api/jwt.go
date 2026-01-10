package api

import (
	"encoding/json"
	"fmt"

	"github.com/golang-jwt/jwt/v5"
)

type JWTUserInfo struct {
	UserID       string `json:"user_id"`
	Email        string `json:"email"`
	DeviceId     string `json:"device_id"`
	LegacyUserID int64  `json:"legacy_user_id"`
	LegacyToken  string `json:"legacy_token"`
}

func decodeJWT(tokenStr string) (*JWTUserInfo, error) {
	claims := jwt.MapClaims{}
	token, _, err := new(jwt.Parser).ParseUnverified(tokenStr, &claims)
	if err != nil {
		return nil, err
	}
	// Convert MapClaims to JSON
	claimsJSON, err := json.Marshal(token.Claims)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal claims: %v", err)
	}
	var userInfo JWTUserInfo
	if err := json.Unmarshal(claimsJSON, &userInfo); err != nil {
		return nil, fmt.Errorf("failed to unmarshal to JWTUserInfo: %v", err)
	}

	return &userInfo, nil
}
