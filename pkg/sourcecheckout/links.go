package sourcecheckout

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
)

type linkTree struct {
	entries     map[string]Entry
	directories map[string]bool
}

// Resolve components in filesystem order. Cleaning alias/../file first would
// discard the alias before its target determines what '..' actually means.
func requestedLinkTarget(entry Entry, tree linkTree, l Limits) (string, error) {
	pending := strings.Split(entry.Link, "/")
	current := path.Dir(entry.Path)
	if current == "." {
		current = ""
	}
	followed, steps := 0, 0
	for len(pending) > 0 {
		component := pending[0]
		pending = pending[1:]
		steps++
		if steps > 4096 {
			return "", fmt.Errorf("source link resolution exceeds bound")
		}
		switch component {
		case "", ".":
			continue
		case "..":
			if current == "" {
				return "", fmt.Errorf("source link escapes root")
			}
			current = path.Dir(current)
			if current == "." {
				current = ""
			}
			continue
		}
		candidate := path.Join(current, component)
		if err := allowedLinkComponent(candidate, l); err != nil {
			return "", err
		}
		leaf, present := tree.entries[candidate]
		if present && leaf.Kind == "symlink" {
			followed++
			if followed > 40 {
				return "", fmt.Errorf("source link chain exceeds bound")
			}
			expanded := strings.Split(leaf.Link, "/")
			if len(expanded) > 4096-len(pending) {
				return "", fmt.Errorf("source link expansion exceeds bound")
			}
			pending = append(expanded, pending...)
			current = path.Dir(candidate)
			if current == "." {
				current = ""
			}
			continue
		}
		if present {
			if len(pending) > 0 {
				return "", fmt.Errorf("source link crosses a file")
			}
			return candidate, nil
		}
		if !tree.directories[candidate] {
			return "", fmt.Errorf("source link uses unlisted target or alias: %s", candidate)
		}
		current = candidate
	}
	if err := allowed(current, l); err != nil {
		return "", err
	}
	return current, nil
}

func allowedLinkComponent(name string, l Limits) error {
	if err := allowed(name, Limits{}); err != nil {
		return err
	}
	folded := foldSourcePath(name)
	for _, protected := range l.Protected {
		prefix := foldSourcePath(protected)
		if folded == prefix || strings.HasPrefix(folded, prefix+"/") {
			return fmt.Errorf("source link enters protected state")
		}
	}
	return nil
}

// A resolved target is checked without following any filesystem symlink.
// All aliases come from the requested inventory, whose actual staging metadata
// was checked by prepare/BuildEnvelope. Destination transitions are checked
// against explicit donor ownership before any write.
func checkResolvedTarget(staging, destination, target string, tree linkTree, old map[string]Entry) error {
	if err := realParents(staging, target); err != nil {
		return err
	}
	info, err := os.Lstat(filepath.Join(staging, target))
	if err != nil {
		return err
	}
	if tree.directories[target] {
		if !info.IsDir() {
			return fmt.Errorf("staging link directory type mismatch")
		}
	} else if !info.Mode().IsRegular() {
		return fmt.Errorf("staging link file type mismatch")
	}
	if destination != "" {
		return checkDestination(destination, target, old)
	}
	return nil
}

type linkDirectoryChecks struct {
	remaining int
	visited   map[string]bool
	requested linkTree
	previous  linkTree
}

// Directory targets must not expose unlisted children, including aliases into
// protected state. WalkDir never follows links; every requested link is resolved
// separately. Donor leaves scheduled for deletion are allowed in destination.
func (checks *linkDirectoryChecks) directory(root, target string, donor bool) error {
	key := root + "\x00" + target
	if checks.visited[key] {
		return nil
	}
	checks.visited[key] = true
	filename := filepath.Join(root, target)
	info, err := os.Lstat(filename)
	if donor && os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if donor && !info.IsDir() {
		if _, owned := checks.previous.entries[target]; owned {
			return nil
		}
	}
	return filepath.WalkDir(filename, func(filename string, item os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		checks.remaining--
		if checks.remaining < 0 {
			return fmt.Errorf("source link directory scan exceeds bound")
		}
		relative, err := filepath.Rel(root, filename)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if item.IsDir() {
			if checks.requested.directories[relative] || donor && checks.previous.directories[relative] {
				return nil
			}
		} else {
			if _, listed := checks.requested.entries[relative]; listed {
				return nil
			}
			if _, owned := checks.previous.entries[relative]; donor && owned {
				return nil
			}
		}
		return fmt.Errorf("source link directory exposes unlisted content: %s", relative)
	})
}

func validateLinks(staging, destination string, previous, next Envelope, l Limits) error {
	wanted := entriesByPath(next)
	old := entriesByPath(previous)
	directories, err := donorDirectories(next)
	if err != nil {
		return err
	}
	oldDirectories, err := donorDirectories(previous)
	if err != nil {
		return err
	}
	tree := linkTree{entries: wanted, directories: directories}
	checks := linkDirectoryChecks{remaining: 40000, visited: make(map[string]bool), requested: tree, previous: linkTree{entries: old, directories: oldDirectories}}
	for _, entry := range next.Entries {
		if entry.Kind != "symlink" {
			continue
		}
		target, err := requestedLinkTarget(entry, tree, l)
		if err != nil {
			return err
		}
		if err := checkResolvedTarget(staging, destination, target, tree, old); err != nil {
			return err
		}
		if directories[target] {
			if err := checks.directory(staging, target, false); err != nil {
				return err
			}
			if destination != "" {
				if err := checks.directory(destination, target, true); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
