//go:build linux

package artifactcache

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Run with DAC_OVERRIDE absent so a read-only payload forces a real partial
// recursive deletion rather than an injected error before any filesystem work.
func TestToolRetirementFailureRemainsRecoverable(t *testing.T) {
	if os.Geteuid() == 0 && os.Getenv("ACT_TEST_RETIREMENT_NO_DAC") != "1" {
		binary, err := os.Executable()
		require.NoError(t, err)
		command := exec.Command(binary, "-test.run", "^TestToolRetirementFailureRemainsRecoverable$", "-test.v")
		command.Env = append(os.Environ(), "ACT_TEST_RETIREMENT_NO_DAC=1", "TMPDIR=/tmp")
		command.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: 65534, Gid: 65534}}
		output, err := command.CombinedOutput()
		require.NoError(t, err, string(output))
		t.Log(string(output))
		return
	}
	for _, kind := range []string{"object", "generation"} {
		t.Run(kind, func(t *testing.T) { toolRetirementFailureCase(t, kind) })
	}
}

func toolRetirementFailureCase(t *testing.T, kind string) {
	t.Helper()
	ctx := context.Background()
	source, store := completedToolFixture(t)
	warm := PublishToolSnapshot(ctx, source, store, 10000)
	require.False(t, warm.Partial, warm.Error)
	selected := InitializeToolGeneration(ctx, store, ToolGenerationSpec{SchemaVersion: 1, Installs: []ToolGenerationInstall{{Path: "Go/1/x64", ObjectID: warm.ID}}}, 10000)
	require.False(t, selected.Partial, selected.Error)
	directory := filepath.Join(source, "readonly")
	require.NoError(t, os.Mkdir(directory, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(directory, "payload"), []byte("orphan"), 0600))
	require.NoError(t, os.Chmod(directory, 0555))
	t.Cleanup(func() { _ = os.Chmod(directory, 0755) })
	orphan := PublishToolSnapshot(ctx, source, store, 10000)
	require.False(t, orphan.Partial, orphan.Error)
	t.Cleanup(func() {
		_ = filepath.WalkDir(store, func(path string, entry os.DirEntry, err error) error {
			if err == nil && entry.IsDir() {
				_ = chmodRetirementFixtureDirectory(store, path)
			}
			return nil
		})
	})
	destination := orphan.Destination
	retire := func() error { return RetireToolObject(ctx, store, orphan.ID, 10000, 100) }
	if kind == "generation" {
		abandoned := UpdateToolGeneration(ctx, store, ToolGenerationSpec{SchemaVersion: 1, Installs: []ToolGenerationInstall{{Path: "Go/1/x64", ObjectID: orphan.ID}}}, 10000)
		require.False(t, abandoned.Partial, abandoned.Error)
		restored := UpdateToolGeneration(ctx, store, ToolGenerationSpec{SchemaVersion: 1, Installs: []ToolGenerationInstall{{Path: "Go/1/x64", ObjectID: warm.ID}}}, 10000)
		require.False(t, restored.Partial, restored.Error)
		destination = abandoned.Generation.Destination
		retire = func() error { return RetireToolGeneration(ctx, store, abandoned.Generation.ID, 10000) }
	}
	require.ErrorContains(t, retire(), "stage retirement incomplete")
	_, err := os.Lstat(destination)
	require.True(t, os.IsNotExist(err), "retirement must move the object out of its published namespace before deleting any payload")
	catalog, err := prepareExistingToolSnapshotStore(store)
	require.NoError(t, err)
	rows, err := loadToolStageOwnership(catalog, store)
	require.NoError(t, err)
	require.Len(t, rows, 1, "failed deletion must retain original inode ownership")
	stage := filepath.Join(store, rows[0].Relative)
	require.NoError(t, catalog.Close())
	_, err = os.Lstat(filepath.Join(stage, "manifest.json"))
	require.True(t, os.IsNotExist(err), "real deletion removed the manifest; recovery must rely on the ledger")
	require.NoError(t, filepath.WalkDir(stage, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return chmodRetirementFixtureDirectory(stage, path)
		}
		return nil
	}))
	recovered := RetireToolStages(ctx, store, time.Now().Add(time.Hour), 10000)
	require.False(t, recovered.Partial, recovered.Error)
	require.Contains(t, recovered.RetiredStages, stage)
	_, err = os.Lstat(stage)
	require.True(t, os.IsNotExist(err))
	current, err := CurrentToolGeneration(ctx, store, 10000)
	require.NoError(t, err)
	require.Equal(t, selected.Generation.ID, current.ID)
}

func TestToolRetirementBeforeRenamePreservesPublication(t *testing.T) {
	ctx := context.Background()
	store, spec := toolGenerationFixture(t)
	initialized := InitializeToolGeneration(ctx, store, spec, 100)
	require.False(t, initialized.Partial, initialized.Error)
	catalog, err := prepareExistingToolSnapshotStore(store)
	require.NoError(t, err)
	publication := filepath.Join(store, spec.Installs[0].ObjectID)
	_, err = registerToolRetirementStage(store, publication)
	require.NoError(t, err)
	require.NoError(t, catalog.Close())
	swept := RetireToolStages(ctx, store, time.Now().Add(time.Hour), 10000)
	require.False(t, swept.Partial, swept.Error)
	require.Empty(t, swept.RetiredStages, "record alone cannot authorize deletion of the original publication")
	current, err := CurrentToolGeneration(ctx, store, 100)
	require.NoError(t, err)
	require.Equal(t, initialized.Generation.ID, current.ID)
	catalog, err = prepareExistingToolSnapshotStore(store)
	require.NoError(t, err)
	defer catalog.Close()
	rows, err := loadToolStageOwnership(catalog, store)
	require.NoError(t, err)
	require.Empty(t, rows)
}

func TestToolStageRecoversInterruptedOwnershipRecord(t *testing.T) {
	store, _ := toolGenerationFixture(t)
	catalog, err := prepareExistingToolSnapshotStore(store)
	require.NoError(t, err)
	stage, err := createOwnedToolStage(catalog, store, store, ".tool-stage-")
	require.NoError(t, err)
	pending := filepath.Join(store, toolStageOwnershipDirectory, ".pending")
	require.NoError(t, os.WriteFile(pending, []byte(`{"schema_version":`), 0600))
	require.NoError(t, catalog.Close())
	report := RetireToolStages(context.Background(), store, time.Now().Add(time.Hour), 10000)
	require.False(t, report.Partial, report.Error)
	require.Contains(t, report.RetiredStages, stage)
	_, err = os.Lstat(pending)
	require.True(t, os.IsNotExist(err))
}

func TestToolStageRecoveryUsesCurrentMountBoundary(t *testing.T) {
	store, _ := toolGenerationFixture(t)
	catalog, err := prepareExistingToolSnapshotStore(store)
	require.NoError(t, err)
	stage, err := createOwnedToolStage(catalog, store, store, ".tool-stage-")
	require.NoError(t, err)
	rows, err := loadToolStageOwnership(catalog, store)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	row := rows[0]
	row.MountID++ // A mount identifier is not durable across helper namespaces.
	data, err := json.Marshal(row)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(store, toolStageOwnershipDirectory, toolStageRecordName(row.Relative)), data, 0600))
	require.NoError(t, catalog.Close())
	report := RetireToolStages(context.Background(), store, time.Now().Add(time.Hour), 10000)
	require.False(t, report.Partial, report.Error)
	require.Contains(t, report.RetiredStages, stage)
}

func chmodRetirementFixtureDirectory(base, path string) error {
	root, err := os.OpenRoot(base)
	if err != nil {
		return err
	}
	defer root.Close()
	relative, err := filepath.Rel(base, path)
	if err != nil {
		return err
	}
	return root.Chmod(relative, 0755)
}

func TestToolStageRecoveryPreservesChangedRootProof(t *testing.T) {
	store, _ := toolGenerationFixture(t)
	catalog, err := prepareExistingToolSnapshotStore(store)
	require.NoError(t, err)
	stage, err := createOwnedToolStage(catalog, store, store, ".tool-stage-")
	require.NoError(t, err)
	rows, err := loadToolStageOwnership(catalog, store)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	row := rows[0]
	row.RootInode++
	data, err := json.Marshal(row)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(store, toolStageOwnershipDirectory, toolStageRecordName(row.Relative)), data, 0600))
	require.NoError(t, catalog.Close())
	report := RetireToolStages(context.Background(), store, time.Now().Add(time.Hour), 10000)
	require.True(t, report.Partial)
	require.Contains(t, report.Error, "root identity changed")
	_, err = os.Lstat(stage)
	require.NoError(t, err, "replacement root must not inherit deletion authority")
}

func TestToolStagePendingRecordPreservesUnexpectedEntries(t *testing.T) {
	for _, kind := range []string{"symlink", "directory", "oversized"} {
		t.Run(kind, func(t *testing.T) {
			store, _ := toolGenerationFixture(t)
			catalog, err := prepareExistingToolSnapshotStore(store)
			require.NoError(t, err)
			stage, err := createOwnedToolStage(catalog, store, store, ".tool-stage-")
			require.NoError(t, err)
			pending := filepath.Join(store, toolStageOwnershipDirectory, toolStagePendingRecord)
			switch kind {
			case "symlink":
				require.NoError(t, os.Symlink(stage, pending))
			case "directory":
				require.NoError(t, os.Mkdir(pending, 0700))
			case "oversized":
				require.NoError(t, os.WriteFile(pending, make([]byte, 1025), 0600))
			}
			require.NoError(t, catalog.Close())
			report := RetireToolStages(context.Background(), store, time.Now().Add(time.Hour), 10000)
			require.True(t, report.Partial)
			require.Contains(t, report.Error, "unknown pending stage record preserved")
			_, err = os.Lstat(pending)
			require.NoError(t, err)
			_, err = os.Lstat(stage)
			require.NoError(t, err)
		})
	}
}

func TestToolStageRecoversInterruptedEmptyAllocation(t *testing.T) {
	store, _ := toolGenerationFixture(t)
	catalog, err := prepareExistingToolSnapshotStore(store)
	require.NoError(t, err)
	stage, err := createOwnedToolStage(catalog, store, store, ".tool-stage-")
	require.NoError(t, err)
	directory := filepath.Join(store, toolStageOwnershipDirectory)
	creating := filepath.Join(directory, ".creating")
	require.NoError(t, os.Mkdir(creating, 0700))
	require.NoError(t, os.WriteFile(filepath.Join(directory, toolStagePendingRecord), []byte(`{"schema_version":`), 0600))
	require.NoError(t, catalog.Close())
	report := RetireToolStages(context.Background(), store, time.Now().Add(time.Hour), 10000)
	require.False(t, report.Partial, report.Error)
	require.Contains(t, report.RetiredStages, stage)
	_, err = os.Lstat(creating)
	require.True(t, os.IsNotExist(err), "reserved allocation never receives payload before authority and rename")
}
