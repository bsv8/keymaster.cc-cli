package connect

import (
	"fmt"
	"github.com/bsv8/keymaster.cc-cli/apppublication"
)

// Publication identity is checked against the Receiver's trusted origin.
// publicationProof 是完整发布记录字符串；后台解码、验签并核对来源，不再解析对象形态。

// VerifiedPublication 解码并验签当前来源的发布记录字符串，返回已验证发布。
// present 为 false 表示请求未携带 publicationProof 字段。
func VerifiedPublication(params map[string]any, eventOrigin string) (*apppublication.Publication, bool, error) {
	raw, present := params["publicationProof"]
	if !present || raw == nil {
		return nil, false, nil
	}
	content, ok := raw.(string)
	if !ok || content == "" {
		return nil, true, fmt.Errorf("publicationProof must be a non-empty record string")
	}
	proof, err := apppublication.DecodePublication(content)
	if err != nil {
		return nil, true, err
	}
	canonical, err := CanonicalOrigin(eventOrigin)
	if err != nil {
		return nil, true, fmt.Errorf("invalid trusted origin")
	}
	p, err := apppublication.VerifyPublication(proof, canonical)
	if err != nil {
		return nil, true, err
	}
	return p, true, nil
}
