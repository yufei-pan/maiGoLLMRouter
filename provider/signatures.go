package provider

import (
	"encoding/json"
	"strings"
	"sync"
	"time"
)

// geminiPlaceholderSignature is Google's documented stand-in for a function
// call whose thought signature is unknown. Gemini 3 accepts it where it would
// refuse a missing signature, at some cost to reasoning quality.
const geminiPlaceholderSignature = "skip_thought_signature_validator"

// SignatureCache remembers Gemini thought signatures by the tool-call id the
// router issued, for clients that drop the signature from their history. A
// signature is only given back to the provider/model that wrote it.
type SignatureCache struct {
	now   func() time.Time
	mu    sync.Mutex
	max   int
	ttl   time.Duration
	byID  map[string]sigEntry
	order []string // ids, oldest first
}

type sigEntry struct {
	signer, signature string
	at                time.Time
}

// NewSignatureCache holds up to max signatures, each for up to ttl.
func NewSignatureCache(max int, ttl time.Duration) *SignatureCache {
	return &SignatureCache{now: time.Now, max: max, ttl: ttl, byID: make(map[string]sigEntry)}
}

// Put records the signature of tool call callID, written by signer
// ("provider/model"). A nil cache ignores it.
func (c *SignatureCache) Put(callID, signer, signature string) {
	if c == nil || callID == "" || signature == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.byID[callID]; !ok {
		c.order = append(c.order, callID)
	}
	c.byID[callID] = sigEntry{signer: signer, signature: signature, at: c.now()}
	for len(c.order) > c.max {
		delete(c.byID, c.order[0])
		c.order = c.order[1:]
	}
}

// Get returns the signature of tool call callID when signer wrote it and it
// has not expired, else "".
func (c *SignatureCache) Get(callID, signer string) string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.byID[callID]
	if !ok || e.signer != signer || c.now().Sub(e.at) > c.ttl {
		return ""
	}
	return e.signature
}

// rememberGeminiSignatures stores the thought signature of each tool call in a
// translated Gemini reply.
func rememberGeminiSignatures(openAIBody []byte, req Request) {
	if req.Signatures == nil {
		return
	}
	var parsed struct {
		Choices []struct {
			Message struct {
				ToolCalls []struct {
					ID               string `json:"id"`
					ThoughtSignature string `json:"thought_signature"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if json.Unmarshal(openAIBody, &parsed) != nil {
		return
	}
	for _, c := range parsed.Choices {
		for _, tc := range c.Message.ToolCalls {
			req.Signatures.Put(tc.ID, req.signer(), tc.ThoughtSignature)
		}
	}
}

// isGeminiModel reports a Gemini model (not Gemma), the ones that check
// thought signatures.
func isGeminiModel(model string) bool {
	return strings.HasPrefix(strings.TrimPrefix(model, "models/"), "gemini-")
}
