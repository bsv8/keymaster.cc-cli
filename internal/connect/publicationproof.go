package connect

import (
	"encoding/json"
	"fmt"
	"github.com/bsv8/keymaster.cc-cli/apppublication"
)

// Publication identity is checked against the Receiver's trusted origin.
func VerifyLoginPublicationProof(params map[string]any, eventOrigin string) (string, bool, error) {
	raw, ok := params["publicationProof"]
	if !ok || raw == nil {
		return "", false, nil
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return "", true, err
	}
	proof, err := apppublication.DecodeProofJSON(encoded)
	if err != nil {
		return "", true, err
	}
	canonical, err := CanonicalOrigin(eventOrigin)
	if err != nil {
		return "", true, fmt.Errorf("invalid trusted origin")
	}
	p, err := apppublication.VerifyPublication(proof, canonical)
	if err != nil {
		return "", true, err
	}
	return p.AppPublicKeyHex, true, nil
}
