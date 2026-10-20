package service

import (
	"slices"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/ratio_setting"

	"github.com/gin-gonic/gin"
)

// maxModelNameSuggestions caps how many names an unknown-model error lists.
const maxModelNameSuggestions = 3

// DiagnoseUnknownModel tells an unknown model name apart from a known model
// whose channels are all disabled or filtered out. It returns unknown=true only
// when no channel of the groups the request routes through declares the name,
// enabled or not. suggestions lists enabled model names of those groups that
// look like what the caller meant and that allowed accepts (nil accepts all).
// A lookup failure reports the name as known, so the caller keeps the generic
// "no available channel" wording rather than claiming the model does not exist.
func DiagnoseUnknownModel(c *gin.Context, usingGroup, modelName string, allowed func(string) bool) (unknown bool, suggestions []string) {
	groups := []string{usingGroup}
	if usingGroup == "auto" {
		groups = GetRequestAutoGroups(c, common.GetContextKeyString(c, constant.ContextKeyUserGroup))
	}
	if len(groups) == 0 {
		return false, nil
	}
	routingName := ratio_setting.RoutingMatchModelName(modelName)
	var candidates []string
	for _, group := range groups {
		availability, err := model.GetGroupModelAvailability(group)
		if err != nil {
			return false, nil
		}
		if _, known := availability[modelName]; known {
			return false, nil
		}
		if _, known := availability[routingName]; known {
			return false, nil
		}
		for name, enabled := range availability {
			if enabled && (allowed == nil || allowed(name)) && !slices.Contains(candidates, name) {
				candidates = append(candidates, name)
			}
		}
	}
	return true, SuggestModelNames(modelName, candidates)
}

// SuggestModelNames returns the candidates that look like a mistyped requested.
// Names match first when they are equal after lowercasing and dropping the
// separators "-", "_", "." and spaces (gpt-image2.5-2k → gpt-image-2.5-2K).
// Only when nothing matches that way, a candidate within a small edit distance
// of the normalized name is offered; short names get no edit-distance guess
// because nearly any short name is a few edits from another.
func SuggestModelNames(requested string, candidates []string) []string {
	key := modelNameMatchKey(requested)
	if key == "" {
		return nil
	}
	var exact []string
	for _, candidate := range candidates {
		if candidate != requested && modelNameMatchKey(candidate) == key {
			exact = append(exact, candidate)
		}
	}
	if len(exact) > 0 {
		slices.Sort(exact)
		return exact[:min(len(exact), maxModelNameSuggestions)]
	}

	keyLength := len([]rune(key))
	if keyLength < 5 {
		return nil
	}
	maxDistance := 1
	if keyLength >= 10 {
		maxDistance = 2
	}
	best := maxDistance + 1
	var nearest []string
	for _, candidate := range candidates {
		distance := modelNameEditDistance(key, modelNameMatchKey(candidate))
		switch {
		case distance < best:
			best = distance
			nearest = []string{candidate}
		case distance == best:
			nearest = append(nearest, candidate)
		}
	}
	if best > maxDistance {
		return nil
	}
	slices.Sort(nearest)
	return nearest[:min(len(nearest), maxModelNameSuggestions)]
}

func modelNameMatchKey(name string) string {
	var builder strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		switch r {
		case '-', '_', '.', ' ':
			continue
		}
		builder.WriteRune(r)
	}
	return builder.String()
}

// modelNameEditDistance is the Levenshtein distance between two strings.
func modelNameEditDistance(a, b string) int {
	ar, br := []rune(a), []rune(b)
	previous := make([]int, len(br)+1)
	current := make([]int, len(br)+1)
	for j := range previous {
		previous[j] = j
	}
	for i := range ar {
		current[0] = i + 1
		for j := range br {
			cost := 1
			if ar[i] == br[j] {
				cost = 0
			}
			current[j+1] = min(previous[j+1]+1, current[j]+1, previous[j]+cost)
		}
		previous, current = current, previous
	}
	return previous[len(br)]
}
