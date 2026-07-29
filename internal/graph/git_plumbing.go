package graph

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/spolnik/RepoKarta/internal/catalog"
	"github.com/spolnik/RepoKarta/internal/gitexec"
)

func listFiles(ctx context.Context, repository catalog.Repository, revision string) ([]string, bool, error) {
	output, err := gitOutput(ctx, repository, "ls-tree", "-r", "--name-only", revision)
	if err != nil {
		return nil, false, err
	}
	scanner := bufio.NewScanner(bytes.NewReader(output))
	scanner.Buffer(make([]byte, 64*1024), 2<<20)
	files := make([]string, 0)
	truncated := false
	for scanner.Scan() {
		filePath := strings.TrimSpace(strings.ReplaceAll(scanner.Text(), "\\", "/"))
		if filePath == "" {
			continue
		}
		if len(files) >= maximumFiles {
			truncated = true
			break
		}
		files = append(files, filePath)
	}
	return files, truncated, scanner.Err()
}

func readFile(ctx context.Context, repository catalog.Repository, revision, filePath string) ([]byte, error) {
	sizeOutput, err := gitOutput(ctx, repository, "cat-file", "-s", revision+":"+filePath)
	if err != nil {
		return nil, err
	}
	size, err := strconv.ParseInt(strings.TrimSpace(string(sizeOutput)), 10, 64)
	if err != nil || size < 0 || size > maximumSourceFileSize {
		return nil, errors.New("source file is outside graph analysis bounds")
	}
	return gitOutput(ctx, repository, "cat-file", "blob", revision+":"+filePath)
}

func readFiles(
	ctx context.Context,
	repository catalog.Repository,
	revision string,
	filePaths []string,
) (map[string][]byte, error) {
	output := make(map[string][]byte, len(filePaths))
	if len(filePaths) == 0 {
		return output, nil
	}
	repositoryLocation := gitexec.Repository{}
	if repository.Bare {
		repositoryLocation.GitDirectory = repository.Path
	} else {
		repositoryLocation.Directory = repository.Path
	}
	invocation := gitexec.New(ctx, gitexec.Options{
		Repository: repositoryLocation,
		Timeout:    commandTimeout,
	}, "cat-file", "--batch")
	defer invocation.Close()
	command := invocation.Command
	stdin, err := command.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("open git cat-file input: %w", err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("open git cat-file output: %w", err)
	}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		return nil, fmt.Errorf("start git cat-file batch: %w", err)
	}
	writer := bufio.NewWriter(stdin)
	reader := bufio.NewReader(stdout)
	var batchErr error
	for _, filePath := range filePaths {
		if _, err := fmt.Fprintf(writer, "%s:%s\n", revision, filePath); err != nil {
			batchErr = err
			break
		}
		if err := writer.Flush(); err != nil {
			batchErr = err
			break
		}
		header, err := reader.ReadString('\n')
		if err != nil {
			batchErr = err
			break
		}
		fields := strings.Fields(header)
		if len(fields) == 2 && fields[1] == "missing" {
			continue
		}
		if len(fields) != 3 || fields[1] != "blob" {
			batchErr = fmt.Errorf("unexpected git cat-file header %q", strings.TrimSpace(header))
			break
		}
		size, err := strconv.ParseInt(fields[2], 10, 64)
		if err != nil || size < 0 {
			batchErr = fmt.Errorf("invalid git cat-file size %q", fields[2])
			break
		}
		if size > maximumSourceFileSize {
			_, err = io.CopyN(io.Discard, reader, size)
		} else {
			content := make([]byte, size)
			_, err = io.ReadFull(reader, content)
			if err == nil {
				output[filePath] = content
			}
		}
		if err != nil {
			batchErr = err
			break
		}
		delimiter, err := reader.ReadByte()
		if err != nil || delimiter != '\n' {
			if err == nil {
				err = errors.New("git cat-file response is missing its delimiter")
			}
			batchErr = err
			break
		}
	}
	_ = stdin.Close()
	if batchErr != nil && command.Process != nil {
		_ = command.Process.Kill()
	}
	waitErr := command.Wait()
	if batchErr != nil {
		return nil, fmt.Errorf("read git cat-file batch: %w", batchErr)
	}
	if waitErr != nil {
		return nil, fmt.Errorf("git cat-file batch: %s", firstNonEmpty(strings.TrimSpace(stderr.String()), waitErr.Error()))
	}
	return output, nil
}

func gitOutput(ctx context.Context, repository catalog.Repository, arguments ...string) ([]byte, error) {
	repositoryLocation := gitexec.Repository{}
	if repository.Bare {
		repositoryLocation.GitDirectory = repository.Path
	} else {
		repositoryLocation.Directory = repository.Path
	}
	result, err := gitexec.Run(ctx, gitexec.Options{
		Repository: repositoryLocation,
		Timeout:    commandTimeout,
	}, arguments...)
	if err != nil {
		return nil, err
	}
	return result.Stdout, nil
}
