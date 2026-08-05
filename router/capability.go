package router

import "sync"

// responsesCapability remembers which providers proved, by probe, to have no
// /responses route. Caching the verdict keeps later Responses requests from
// paying for a doomed probe on every call. It only holds negative results:
// a provider that answered on /responses is simply never recorded.
//
// The cache is discarded on config reload, so a downstream that gained the
// route (or a corrected base_url) is probed again.
type responsesCapability struct {
	mu       sync.Mutex
	chatOnly map[string]bool
}

func newResponsesCapability() *responsesCapability {
	return &responsesCapability{chatOnly: make(map[string]bool)}
}

// IsChatOnly reports whether a probe already found the provider has no
// /responses route.
func (c *responsesCapability) IsChatOnly(provider string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.chatOnly[provider]
}

func (c *responsesCapability) MarkChatOnly(provider string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.chatOnly[provider] = true
}

func (c *responsesCapability) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.chatOnly = make(map[string]bool)
}
