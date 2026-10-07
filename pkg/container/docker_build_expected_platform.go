//go:build !(WITHOUT_DOCKER || !(linux || darwin || windows || netbsd))

package container

import (
	"bytes"
	"strings"

	"github.com/containerd/platforms"
	"github.com/moby/buildkit/frontend/dockerfile/parser"
	"github.com/moby/buildkit/frontend/dockerfile/shell"
	specs "github.com/opencontainers/image-spec/specs-go/v1"
)

type effectiveBuildPlatform struct {
	platform specs.Platform
	known    bool
}
type resolvedBuildStage struct {
	name     string
	expected effectiveBuildPlatform
}

// Unknown extensions or unresolved values do not authorize a network retry.
// Docker's own parser still receives the original context unchanged.
func resolveFinalBuildPlatform(dockerfile []byte, target specs.Platform) effectiveBuildPlatform {
	normalized, err := platforms.Parse(platforms.Format(target))
	if err != nil {
		return effectiveBuildPlatform{}
	}
	target = normalized
	parsed, err := parser.Parse(bytes.NewReader(dockerfile))
	if err != nil {
		return effectiveBuildPlatform{}
	}
	lex := shell.NewLex(parsed.EscapeToken)
	lex.SkipUnsetEnv = true
	var args []string
	var stages []resolvedBuildStage
	for _, node := range parsed.AST.Children {
		if strings.EqualFold(node.Value, "arg") && len(stages) == 0 {
			for arg := node.Next; arg != nil; arg = arg.Next {
				key, value, hasDefault := strings.Cut(arg.Value, "=")
				if !hasDefault {
					continue
				}
				resolved, _, err := lex.ProcessWord(value, shell.EnvsFromSlice(args))
				if err != nil {
					return effectiveBuildPlatform{}
				}
				args = append(args, key+"="+resolved)
			}
		}
		if !strings.EqualFold(node.Value, "from") {
			continue
		}
		stage, ok := resolveBuildStage(node, lex, args, stages, target)
		if !ok {
			return effectiveBuildPlatform{}
		}
		stages = append(stages, stage)
	}
	if len(stages) == 0 {
		return effectiveBuildPlatform{}
	}
	return stages[len(stages)-1].expected
}
func resolveBuildStage(node *parser.Node, lex *shell.Lex, args []string, stages []resolvedBuildStage, target specs.Platform) (resolvedBuildStage, bool) {
	if node.Next == nil {
		return resolvedBuildStage{}, false
	}
	base, _, err := lex.ProcessWord(node.Next.Value, shell.EnvsFromSlice(args))
	if err != nil || strings.Contains(base, "$") {
		return resolvedBuildStage{}, false
	}
	expected := effectiveBuildPlatform{platform: target, known: true}
	for _, previous := range stages {
		if strings.EqualFold(previous.name, base) {
			expected = previous.expected
			break
		}
	}
	for _, flag := range node.Flags {
		value, ok := strings.CutPrefix(flag, "--platform=")
		if !ok {
			return resolvedBuildStage{}, false
		}
		resolved, _, err := lex.ProcessWord(value, shell.EnvsFromSlice(args))
		if err != nil || strings.Contains(resolved, "$") {
			return resolvedBuildStage{}, false
		}
		platform, ok := parseExpectedBuildPlatform(resolved)
		if !ok {
			return resolvedBuildStage{}, false
		}
		expected = effectiveBuildPlatform{platform: platform, known: true}
	}
	stage := resolvedBuildStage{expected: expected}
	if node.Next.Next != nil {
		if !strings.EqualFold(node.Next.Next.Value, "as") || node.Next.Next.Next == nil || node.Next.Next.Next.Next != nil {
			return resolvedBuildStage{}, false
		}
		stage.name = node.Next.Next.Next.Value
	}
	return stage, true
}
func parseExpectedBuildPlatform(value string) (specs.Platform, bool) {
	platform, err := platforms.Parse(value)
	return platform, err == nil
}
