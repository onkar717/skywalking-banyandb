// Licensed to Apache Software Foundation (ASF) under one or more contributor
// license agreements. See the NOTICE file distributed with
// this work for additional information regarding copyright
// ownership. Apache Software Foundation (ASF) licenses this file to you under
// the Apache License, Version 2.0 (the "License"); you may
// not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

// Package backup provides the backup command-line tool.
package backup

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"syscall"
	"time"

	"github.com/benbjohnson/clock"
	"github.com/robfig/cron/v3"
	"github.com/spf13/cobra"
	"go.uber.org/multierr"

	commonv1 "github.com/apache/skywalking-banyandb/api/proto/banyandb/common/v1"
	databasev1 "github.com/apache/skywalking-banyandb/api/proto/banyandb/database/v1"
	"github.com/apache/skywalking-banyandb/pkg/fs/remote"
	"github.com/apache/skywalking-banyandb/pkg/fs/remote/local"
	"github.com/apache/skywalking-banyandb/pkg/grpchelper"
	"github.com/apache/skywalking-banyandb/pkg/logger"
	"github.com/apache/skywalking-banyandb/pkg/timestamp"
)

const snapshotDir = "snapshots"

var scheduleExprMap = map[string]string{
	"hourly": "5 * * * *",
	"daily":  "5 0 * * *",
}

// NewBackupCommand creates a new backup command.
func NewBackupCommand() *cobra.Command {
	var (
		gRPCAddr      string
		enableTLS     bool
		insecure      bool
		cert          string
		streamRoot    string
		measureRoot   string
		propertyRoot  string
		dest          string
		timeStyle     string
		scheduleStyle string
	)

	cmd := &cobra.Command{
		Use:   "backup",
		Short: "Backup BanyanDB snapshots to remote storage",
		RunE: func(_ *cobra.Command, _ []string) error {
			if scheduleStyle == "" {
				return backupAction(dest, gRPCAddr, enableTLS, insecure, cert,
					streamRoot, measureRoot, propertyRoot, timeStyle)
			}
			expr, ok := scheduleExprMap[scheduleStyle]
			if !ok {
				return fmt.Errorf("unsupported schedule style: %s. Only support: hourly or daily", scheduleStyle)
			}
			schedLogger := logger.GetLogger().Named("backup-scheduler")
			clockInstance := clock.New()
			sch := timestamp.NewScheduler(schedLogger, clockInstance)
			cronOptions := cron.Minute | cron.Hour
			err := sch.Register("backup", cronOptions, expr, func(_ time.Time, l *logger.Logger) bool {
				err := backupAction(dest, gRPCAddr, enableTLS, insecure, cert,
					streamRoot, measureRoot, propertyRoot, timeStyle)
				if err != nil {
					l.Error().Err(err).Msg("backup failed")
				} else {
					l.Info().Msg("backup succeeded")
				}
				return true
			})
			if err != nil {
				return err
			}

			sigChan := make(chan os.Signal, 1)
			signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
			schedLogger.Info().Msg("backup scheduler started, press Ctrl+C to exit")
			<-sigChan
			schedLogger.Info().Msg("shutting down backup scheduler...")
			sch.Close()
			return nil
		},
	}

	cmd.Flags().StringVar(&gRPCAddr, "grpc-addr", "127.0.0.1:17912", "gRPC address of the data node")
	cmd.Flags().BoolVar(&enableTLS, "enable-tls", false, "Enable TLS for gRPC connection")
	cmd.Flags().BoolVar(&insecure, "insecure", false, "Skip server certificate verification")
	cmd.Flags().StringVar(&cert, "cert", "", "Path to the gRPC server certificate")
	cmd.Flags().StringVar(&streamRoot, "stream-root-path", "/tmp", "Root directory for stream catalog")
	cmd.Flags().StringVar(&measureRoot, "measure-root-path", "/tmp", "Root directory for measure catalog")
	cmd.Flags().StringVar(&propertyRoot, "property-root-path", "/tmp", "Root directory for property catalog")
	cmd.Flags().StringVar(&dest, "dest", "", "Destination URL (e.g., file:///backups)")
	cmd.Flags().StringVar(&timeStyle, "time-style", "daily", "Time directory style (daily|hourly)")
	cmd.Flags().StringVar(&scheduleStyle, "schedule", "", "Schedule expression for periodic backup. The format is a cron expression \"<minute> <hour>\"")

	return cmd
}

func backupAction(dest, gRPCAddr string, enableTLS, insecure bool, cert,
	streamRoot, measureRoot, propertyRoot, timeStyle string,
) error {
	if dest == "" {
		return errors.New("dest is required")
	}

	fs, err := newFS(dest)
	if err != nil {
		return err
	}
	defer fs.Close()

	snapshots, err := getSnapshots(gRPCAddr, enableTLS, insecure, cert)
	if err != nil {
		return err
	}

	timeDir := getTimeDir(timeStyle)

	for _, snapshot := range snapshots {
		var snapshotDir string
		snapshotDir, err = getSnapshotDir(snapshot, streamRoot, measureRoot, propertyRoot)
		if err != nil {
			logger.Warningf("Failed to get snapshot directory for %s: %v", snapshot.Name, err)
			continue
		}
		multierr.AppendInto(&err, backupSnapshot(fs, snapshotDir, snapshot.Name, timeDir))
	}
	return err
}

func newFS(dest string) (remote.FS, error) {
	u, err := url.Parse(dest)
	if err != nil {
		return nil, fmt.Errorf("invalid dest URL: %w", err)
	}

	switch u.Scheme {
	case "file":
		return local.NewFS(u.Path)
	default:
		return nil, fmt.Errorf("unsupported scheme: %s", u.Scheme)
	}
}

func getSnapshots(gRPCAddr string, enableTLS, insecure bool, cert string) ([]*databasev1.Snapshot, error) {
	opts, err := grpchelper.SecureOptions(nil, enableTLS, insecure, cert)
	if err != nil {
		return nil, err
	}
	connection, err := grpchelper.Conn(gRPCAddr, 10*time.Second, opts...)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to gRPC server: %w", err)
	}
	defer connection.Close()
	ctx := context.Background()
	client := databasev1.NewSnapshotServiceClient(connection)
	snapshotResp, err := client.Snapshot(ctx, &databasev1.SnapshotRequest{})
	if err != nil {
		return nil, fmt.Errorf("failed to request snapshot: %w", err)
	}
	return snapshotResp.Snapshots, nil
}

func getSnapshotDir(snapshot *databasev1.Snapshot, streamRoot, measureRoot, propertyRoot string) (string, error) {
	var baseDir string
	switch snapshot.Catalog {
	case commonv1.Catalog_CATALOG_STREAM:
		baseDir = streamRoot
	case commonv1.Catalog_CATALOG_MEASURE:
		baseDir = measureRoot
	case commonv1.Catalog_CATALOG_PROPERTY:
		baseDir = propertyRoot
	default:
		return "", errors.New("unknown catalog type")
	}

	return filepath.Join(baseDir, snapshotDir, snapshot.Name), nil
}

func getTimeDir(style string) string {
	now := time.Now()
	switch style {
	case "hourly":
		return now.Format("2006-01-02-15")
	default:
		return now.Format("2006-01-02")
	}
}

func backupSnapshot(fs remote.FS, snapshotDir, snapshotName, timeDir string) error {
	localFiles, err := getAllFiles(snapshotDir)
	if err != nil {
		return err
	}

	ctx := context.Background()
	remotePrefix := path.Join(timeDir, snapshotName) + "/"

	remoteFiles, err := fs.List(ctx, remotePrefix)
	if err != nil {
		return err
	}

	for _, relPath := range localFiles {
		remotePath := path.Join(timeDir, snapshotName, relPath)
		if !contains(remoteFiles, remotePath) {
			if err := uploadFile(ctx, fs, snapshotDir, relPath, remotePath); err != nil {
				return err
			}
		}
	}

	deleteOrphanedFiles(ctx, fs, localFiles, remoteFiles, timeDir, snapshotName)
	return nil
}

func getAllFiles(root string) ([]string, error) {
	var files []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			relPath, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			files = append(files, filepath.ToSlash(relPath))
		}
		return nil
	})
	return files, err
}

func uploadFile(ctx context.Context, fs remote.FS, snapshotDir, relPath, remotePath string) error {
	localPath := filepath.Join(snapshotDir, relPath)
	file, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer file.Close()

	return fs.Upload(ctx, remotePath, file)
}

func deleteOrphanedFiles(ctx context.Context, fs remote.FS, localFiles, remoteFiles []string, timeDir, snapshotName string) {
	expected := make(map[string]struct{})
	for _, f := range localFiles {
		expected[path.Join(timeDir, snapshotName, f)] = struct{}{}
	}

	for _, remoteFile := range remoteFiles {
		if _, exists := expected[remoteFile]; !exists {
			if err := fs.Delete(ctx, remoteFile); err != nil {
				logger.Warningf("Warning: failed to delete orphaned file %s: %v\n", remoteFile, err)
			}
		}
	}
}

func contains(slice []string, s string) bool {
	for _, item := range slice {
		if item == s {
			return true
		}
	}
	return false
}
