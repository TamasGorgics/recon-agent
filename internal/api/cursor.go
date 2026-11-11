package api

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"regexp"
	"time"
)

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// listCursor is the decoded keyset position: created_at DESC, id DESC.
type listCursor struct {
	CreatedAt time.Time
	ID        string
}

type cursorJSON struct {
	Time string `json:"t"`
	ID   string `json:"id"`
}

// encodeCursor renders the RFC3339Nano UTC timestamp and UUID as unpadded
// base64url JSON.
func encodeCursor(createdAt time.Time, id string) string {
	payload, _ := json.Marshal(cursorJSON{Time: createdAt.UTC().Format(time.RFC3339Nano), ID: id})
	return base64.RawURLEncoding.EncodeToString(payload)
}

// decodeCursor validates a cursor's structure. Duplicate JSON keys and
// trailing bytes are rejected before the typed decode. It is not
// authenticated; a forged but well-formed cursor only changes the read offset
// of an authorized query.
func decodeCursor(value string) (listCursor, error) {
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return listCursor{}, errors.New("api: cursor is not base64url")
	}
	if err := validateJSONDocument(raw); err != nil {
		return listCursor{}, errors.New("api: cursor payload is invalid")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var payload cursorJSON
	if err := decoder.Decode(&payload); err != nil {
		return listCursor{}, errors.New("api: cursor payload is invalid")
	}
	if payload.Time == "" || payload.ID == "" {
		return listCursor{}, errors.New("api: cursor is incomplete")
	}
	createdAt, err := time.Parse(time.RFC3339Nano, payload.Time)
	if err != nil {
		return listCursor{}, errors.New("api: cursor timestamp is invalid")
	}
	if !uuidPattern.MatchString(payload.ID) {
		return listCursor{}, errors.New("api: cursor id is not a UUID")
	}
	return listCursor{CreatedAt: createdAt.UTC(), ID: payload.ID}, nil
}

// parseListLimit validates a limit query value: 1..200, default 50.
func parseListLimit(values []string) (int, error) {
	if len(values) == 0 {
		return defaultListLimit, nil
	}
	if len(values) != 1 {
		return 0, errors.New("api: limit is repeated")
	}
	limit, err := parseIntStrict(values[0])
	if err != nil || limit < 1 || limit > maxListLimit {
		return 0, errors.New("api: limit must be between 1 and 200")
	}
	return limit, nil
}

const (
	defaultListLimit = 50
	maxListLimit     = 200
)

func parseIntStrict(value string) (int, error) {
	if value == "" {
		return 0, errors.New("api: empty integer")
	}
	negative := false
	start := 0
	if value[0] == '-' {
		negative = true
		start = 1
		if len(value) == 1 {
			return 0, errors.New("api: empty integer")
		}
	}
	n := 0
	for i := start; i < len(value); i++ {
		if value[i] < '0' || value[i] > '9' {
			return 0, errors.New("api: not an integer")
		}
		n = n*10 + int(value[i]-'0')
		if n > maxListLimit {
			return 0, errors.New("api: integer is too large")
		}
	}
	if negative {
		return -n, nil
	}
	return n, nil
}
