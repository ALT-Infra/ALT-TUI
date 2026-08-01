package tooling

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/bluekeyes/go-gitdiff/gitdiff"
	"github.com/google/uuid"
)

type ApplyPatchInput struct {
	Patch string `json:"patch" jsonschema:"description=Complete Git or standard unified text patch. Paths must resolve inside the session workspace."`
}

type ApplyPatchResult struct {
	Changed  []string `json:"changed"`
	Warnings []string `json:"warnings,omitempty"`
}

type patchApplier struct {
	root string
	mu   sync.Mutex
}

type plannedChange struct {
	oldPath string
	newPath string
	content []byte
	mode    os.FileMode
	delete  bool
}

type stagedChange struct {
	plan        plannedChange
	tempPath    string
	oldBackup   string
	newBackup   string
	oldMoved    bool
	newMoved    bool
	newWritten  bool
	createdDirs []string
}

func (a *patchApplier) apply(_ context.Context, input ApplyPatchInput) (ApplyPatchResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if strings.TrimSpace(input.Patch) == "" {
		return ApplyPatchResult{}, fmt.Errorf("patch is required")
	}
	files, preamble, err := gitdiff.Parse(strings.NewReader(input.Patch))
	if err != nil {
		return ApplyPatchResult{}, fmt.Errorf("parse patch: %w", err)
	}
	if strings.TrimSpace(preamble) != "" {
		return ApplyPatchResult{}, fmt.Errorf("patch contains non-patch preamble")
	}
	if len(files) == 0 {
		return ApplyPatchResult{}, fmt.Errorf("patch contains no file changes")
	}

	plans := make([]plannedChange, 0, len(files))
	claimed := make(map[string]struct{})
	for _, file := range files {
		if file.IsBinary || file.BinaryFragment != nil || file.ReverseBinaryFragment != nil {
			return ApplyPatchResult{}, fmt.Errorf("binary patches are not supported")
		}
		if file.IsCopy {
			return ApplyPatchResult{}, fmt.Errorf("copy patches are not supported")
		}
		oldPath, err := a.resolvePatchPath(file.OldName)
		if err != nil {
			return ApplyPatchResult{}, fmt.Errorf("old path %q: %w", file.OldName, err)
		}
		newPath, err := a.resolvePatchPath(file.NewName)
		if err != nil {
			return ApplyPatchResult{}, fmt.Errorf("new path %q: %w", file.NewName, err)
		}
		if file.IsNew {
			oldPath = ""
		}
		if file.IsDelete {
			newPath = ""
		}
		if oldPath == "" && newPath == "" {
			return ApplyPatchResult{}, fmt.Errorf("file change has neither an old nor new path")
		}
		filePaths := map[string]struct{}{}
		for _, path := range []string{oldPath, newPath} {
			if path == "" {
				continue
			}
			if _, sameFile := filePaths[path]; sameFile {
				continue
			}
			filePaths[path] = struct{}{}
			if _, duplicate := claimed[path]; duplicate {
				return ApplyPatchResult{}, fmt.Errorf("patch changes path more than once: %s", a.relative(path))
			}
			claimed[path] = struct{}{}
		}

		var source []byte
		var mode os.FileMode = 0o644
		if oldPath != "" {
			info, statErr := os.Lstat(oldPath)
			if statErr != nil {
				return ApplyPatchResult{}, fmt.Errorf("inspect %s: %w", a.relative(oldPath), statErr)
			}
			if !info.Mode().IsRegular() {
				return ApplyPatchResult{}, fmt.Errorf("patch source is not a regular file: %s", a.relative(oldPath))
			}
			source, err = os.ReadFile(oldPath)
			if err != nil {
				return ApplyPatchResult{}, fmt.Errorf("read %s: %w", a.relative(oldPath), err)
			}
			mode = info.Mode().Perm()
		} else if _, statErr := os.Lstat(newPath); !os.IsNotExist(statErr) {
			if statErr != nil {
				return ApplyPatchResult{}, fmt.Errorf("inspect %s: %w", a.relative(newPath), statErr)
			}
			return ApplyPatchResult{}, fmt.Errorf("new patch path already exists: %s", a.relative(newPath))
		}
		if oldPath != "" && newPath != "" && oldPath != newPath {
			if _, statErr := os.Lstat(newPath); !os.IsNotExist(statErr) {
				if statErr != nil {
					return ApplyPatchResult{}, fmt.Errorf("inspect %s: %w", a.relative(newPath), statErr)
				}
				return ApplyPatchResult{}, fmt.Errorf("patch destination already exists: %s", a.relative(newPath))
			}
		}
		if file.NewMode != 0 {
			mode = file.NewMode.Perm()
		}
		var output bytes.Buffer
		if err := gitdiff.Apply(&output, bytes.NewReader(source), file); err != nil {
			return ApplyPatchResult{}, fmt.Errorf("apply %s: %w", a.relative(firstPath(newPath, oldPath)), err)
		}
		plans = append(plans, plannedChange{
			oldPath: oldPath,
			newPath: newPath,
			content: output.Bytes(),
			mode:    mode,
			delete:  newPath == "",
		})
	}

	staged, err := a.stage(plans)
	if err != nil {
		return ApplyPatchResult{}, err
	}
	if err := a.commit(staged); err != nil {
		a.rollback(staged)
		return ApplyPatchResult{}, err
	}
	changed := make([]string, 0, len(plans))
	for _, plan := range plans {
		changed = append(changed, a.relative(firstPath(plan.newPath, plan.oldPath)))
	}
	var warnings []string
	for _, item := range staged {
		for _, backup := range []string{item.oldBackup, item.newBackup} {
			if backup == "" {
				continue
			}
			if err := os.Remove(backup); err != nil && !os.IsNotExist(err) {
				warnings = append(warnings, fmt.Sprintf(
					"committed changes but could not remove backup %s: %v",
					a.relative(backup),
					err,
				))
			}
		}
	}
	return ApplyPatchResult{Changed: changed, Warnings: warnings}, nil
}

func (a *patchApplier) stage(plans []plannedChange) ([]*stagedChange, error) {
	staged := make([]*stagedChange, 0, len(plans))
	for _, plan := range plans {
		item := &stagedChange{plan: plan}
		if !plan.delete {
			created, err := createPatchParents(a.root, filepath.Dir(plan.newPath))
			if err != nil {
				cleanupStaged(staged)
				return nil, fmt.Errorf("create patch destination directory: %w", err)
			}
			item.createdDirs = created
			temp, err := os.CreateTemp(filepath.Dir(plan.newPath), ".alt-patch-*")
			if err != nil {
				cleanupStaged([]*stagedChange{item})
				cleanupStaged(staged)
				return nil, fmt.Errorf("stage patch destination: %w", err)
			}
			item.tempPath = temp.Name()
			if err := temp.Chmod(plan.mode); err != nil {
				temp.Close()
				cleanupStaged([]*stagedChange{item})
				cleanupStaged(staged)
				return nil, fmt.Errorf("set staged patch mode: %w", err)
			}
			if _, err := temp.Write(plan.content); err != nil {
				temp.Close()
				cleanupStaged([]*stagedChange{item})
				cleanupStaged(staged)
				return nil, fmt.Errorf("write staged patch: %w", err)
			}
			if err := temp.Sync(); err != nil {
				temp.Close()
				cleanupStaged([]*stagedChange{item})
				cleanupStaged(staged)
				return nil, fmt.Errorf("sync staged patch: %w", err)
			}
			if err := temp.Close(); err != nil {
				cleanupStaged([]*stagedChange{item})
				cleanupStaged(staged)
				return nil, fmt.Errorf("close staged patch: %w", err)
			}
		}
		staged = append(staged, item)
	}
	return staged, nil
}

func (a *patchApplier) commit(staged []*stagedChange) error {
	for _, item := range staged {
		plan := item.plan
		if plan.oldPath != "" {
			item.oldBackup = backupPath(plan.oldPath)
			if err := os.Rename(plan.oldPath, item.oldBackup); err != nil {
				return fmt.Errorf("prepare %s for patch: %w", a.relative(plan.oldPath), err)
			}
			item.oldMoved = true
		}
		if plan.newPath != "" && plan.newPath != plan.oldPath {
			if _, err := os.Lstat(plan.newPath); err == nil {
				item.newBackup = backupPath(plan.newPath)
				if err := os.Rename(plan.newPath, item.newBackup); err != nil {
					return fmt.Errorf("prepare patch destination %s: %w", a.relative(plan.newPath), err)
				}
				item.newMoved = true
			} else if !os.IsNotExist(err) {
				return fmt.Errorf("inspect patch destination %s: %w", a.relative(plan.newPath), err)
			}
		}
		if !plan.delete {
			if err := os.Rename(item.tempPath, plan.newPath); err != nil {
				return fmt.Errorf("publish patch destination %s: %w", a.relative(plan.newPath), err)
			}
			item.tempPath = ""
			item.newWritten = true
		}
	}
	return nil
}

func (a *patchApplier) rollback(staged []*stagedChange) {
	for index := len(staged) - 1; index >= 0; index-- {
		item := staged[index]
		if item.newWritten {
			_ = os.Remove(item.plan.newPath)
		}
		if item.newMoved {
			_ = os.Rename(item.newBackup, item.plan.newPath)
		}
		if item.oldMoved {
			_ = os.Rename(item.oldBackup, item.plan.oldPath)
		}
		if item.tempPath != "" {
			_ = os.Remove(item.tempPath)
		}
		removeCreatedDirs(item.createdDirs)
	}
}

func cleanupStaged(staged []*stagedChange) {
	for index := len(staged) - 1; index >= 0; index-- {
		item := staged[index]
		if item.tempPath != "" {
			_ = os.Remove(item.tempPath)
		}
		removeCreatedDirs(item.createdDirs)
	}
}

func createPatchParents(root, directory string) ([]string, error) {
	var missing []string
	current := directory
	for current != root {
		info, err := os.Stat(current)
		if err == nil {
			if !info.IsDir() {
				return nil, fmt.Errorf("path component is not a directory: %s", current)
			}
			break
		}
		if !os.IsNotExist(err) {
			return nil, err
		}
		missing = append(missing, current)
		parent := filepath.Dir(current)
		if parent == current {
			return nil, fmt.Errorf("destination escaped workspace")
		}
		current = parent
	}
	for index := len(missing) - 1; index >= 0; index-- {
		if err := os.Mkdir(missing[index], 0o755); err != nil {
			removeCreatedDirs(missing[index+1:])
			return nil, err
		}
	}
	return missing, nil
}

func removeCreatedDirs(directories []string) {
	for _, directory := range directories {
		_ = os.Remove(directory)
	}
}

func (a *patchApplier) resolvePatchPath(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || name == "/dev/null" {
		return "", nil
	}
	if filepath.IsAbs(name) {
		return "", fmt.Errorf("absolute paths are not allowed")
	}
	candidate := filepath.Clean(filepath.Join(a.root, filepath.FromSlash(name)))
	relative, err := filepath.Rel(a.root, candidate)
	if err != nil {
		return "", err
	}
	if relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return "", fmt.Errorf("path is outside the session workspace")
	}
	resolved, err := resolveExistingAncestor(candidate)
	if err != nil {
		return "", err
	}
	relative, err = filepath.Rel(a.root, resolved)
	if err != nil {
		return "", err
	}
	if relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return "", fmt.Errorf("path resolves outside the session workspace")
	}
	return resolved, nil
}

func (a *patchApplier) relative(path string) string {
	relative, err := filepath.Rel(a.root, path)
	if err != nil {
		return path
	}
	return filepath.ToSlash(relative)
}

func backupPath(path string) string {
	return filepath.Join(filepath.Dir(path), ".alt-patch-backup-"+uuid.NewString())
}

func firstPath(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
