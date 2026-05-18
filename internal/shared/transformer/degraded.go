package transformer


// StripAnthropicFeatures removes cache_control and extended_thinking from all
// content blocks and the request, returning the list of degraded features.
// Called when routing selects a non-Anthropic provider.
func StripAnthropicFeatures(req *IRRequest) []string {
	if req == nil {
		return nil
	}
	var degraded []string

	if req.Thinking != nil && req.Thinking.Enabled {
		degraded = append(degraded, "extended_thinking")
		req.Thinking = nil
	}

	for i := range req.Messages {
		for j := range req.Messages[i].Content {
			if req.Messages[i].Content[j].CacheControl != nil {
				degraded = appendStrUnique(degraded, "cache_control")
				req.Messages[i].Content[j].CacheControl = nil
			}
		}
	}
	for i := range req.System {
		if req.System[i].CacheControl != nil {
			degraded = appendStrUnique(degraded, "cache_control")
			req.System[i].CacheControl = nil
		}
	}
	return degraded
}

func appendStrUnique(slice []string, s string) []string {
	for _, existing := range slice {
		if existing == s {
			return slice
		}
	}
	return append(slice, s)
}
