package main

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/storage"
	"google.golang.org/api/option"
)

func TestUploadFramePackageCreatesDeterministicTarObject(t *testing.T) {
	framePackage := testFramePackageUpload(t)
	device := deviceRecord{id: 83, organisationID: 42, siteID: 17}
	var (
		server       *httptest.Server
		mutex        sync.Mutex
		requests     []string
		uploadedBody []byte
		contentType  string
	)
	server = httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		request *http.Request,
	) {
		mutex.Lock()
		requests = append(requests, request.Method+" "+request.URL.RequestURI())
		mutex.Unlock()
		switch {
		case request.Method == http.MethodPost:
			if got := request.URL.Query().Get("ifGenerationMatch"); got != "0" {
				t.Errorf("ifGenerationMatch = %q, want 0", got)
			}
			if got := request.URL.Query().Get("name"); got != framePackageObjectName(
				device.organisationID,
				device.siteID,
				device.id,
				framePackage.window,
			) {
				t.Errorf("object name = %q", got)
			}
			if request.URL.Query().Get("uploadType") == "multipart" {
				var err error
				uploadedBody, contentType, err = readMultipartUpload(request)
				if err != nil {
					t.Error(err)
				}
				writer.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(writer, `{"kind":"storage#object","bucket":"camos-prod-0"}`)
				return
			}
			contentType = request.Header.Get("X-Upload-Content-Type")
			writer.Header().Set("Location", server.URL+"/upload-session?upload_id=phase7")
			writer.Header().Set("X-GUploader-UploadID", "phase7")
		case request.Method == http.MethodPut && request.URL.Path == "/upload-session":
			var err error
			uploadedBody, err = io.ReadAll(request.Body)
			if err != nil {
				t.Error(err)
			}
			writer.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(writer, `{"kind":"storage#object","bucket":"camos-prod-0"}`)
		default:
			http.Error(writer, "unexpected request", http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := newTestStorageClient(t, server.URL)

	completedAt, err := uploadFramePackage(
		context.Background(),
		client,
		device,
		framePackage,
	)
	if err != nil {
		t.Fatalf("uploadFramePackage() error = %v", err)
	}
	if completedAt.IsZero() {
		t.Fatal("successful upload returned a zero completion time")
	}
	if contentType != "application/x-tar" {
		t.Fatalf("upload content type = %q", contentType)
	}
	mutex.Lock()
	requestSnapshot := append([]string(nil), requests...)
	mutex.Unlock()
	if len(requestSnapshot) < 1 || len(requestSnapshot) > 2 ||
		!strings.HasPrefix(requestSnapshot[0], "POST /upload/storage/v1/b/"+gcsBucketName+"/o?") ||
		(len(requestSnapshot) == 2 && !strings.HasPrefix(requestSnapshot[1], "PUT /upload-session?")) {
		t.Fatalf("upload requests = %#v", requestSnapshot)
	}

	reader := tar.NewReader(bytes.NewReader(uploadedBody))
	header, err := reader.Next()
	if err != nil {
		t.Fatalf("uploaded object is not a TAR: %v", err)
	}
	if header.Name != "2026-09-06T11-07-03.456Z.jpg" {
		t.Fatalf("TAR entry = %q", header.Name)
	}
	payload, err := io.ReadAll(reader)
	if err != nil || string(payload) != "jpeg-payload" {
		t.Fatalf("TAR payload = %q, %v", payload, err)
	}
}

func TestPreconditionFailureCompletesCreateWithoutRead(t *testing.T) {
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		request *http.Request,
	) {
		requests = append(requests, request.Method+" "+request.URL.Path)
		if got := request.URL.Query().Get("ifGenerationMatch"); got != "0" {
			t.Errorf("ifGenerationMatch = %q, want 0", got)
		}
		writeStorageError(writer, http.StatusPreconditionFailed)
	}))
	defer server.Close()
	client := newTestStorageClient(t, server.URL)

	completedAt, err := uploadFramePackage(
		context.Background(),
		client,
		deviceRecord{id: 83, organisationID: 42, siteID: 17},
		testFramePackageUpload(t),
	)
	if err != nil {
		t.Fatalf("uploadFramePackage() error = %v", err)
	}
	if completedAt.IsZero() {
		t.Fatal("precondition completion has a zero completion time")
	}
	if len(requests) != 1 || !strings.HasPrefix(requests[0], "POST ") {
		t.Fatalf("requests = %#v, want one create request and no read", requests)
	}
}

func TestAmbiguousCreateRetryUsesSameTargetThenAcceptsPrecondition(t *testing.T) {
	var (
		server    *httptest.Server
		mutex     sync.Mutex
		postNames []string
	)
	server = httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		request *http.Request,
	) {
		if request.Method == http.MethodPost {
			mutex.Lock()
			postNames = append(postNames, request.URL.Query().Get("name"))
			attempt := len(postNames)
			mutex.Unlock()
			if attempt == 2 {
				writeStorageError(writer, http.StatusPreconditionFailed)
				return
			}
			hijacker, ok := writer.(http.Hijacker)
			if !ok {
				t.Error("test server does not support connection hijacking")
				return
			}
			connection, _, err := hijacker.Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			_ = connection.Close()
			return
		}
		http.Error(writer, "unexpected request", http.StatusNotFound)
	}))
	defer server.Close()
	client := newTestStorageClient(t, server.URL)
	device := deviceRecord{id: 83, organisationID: 42, siteID: 17}
	framePackage := testFramePackageUpload(t)

	if _, err := uploadFramePackage(context.Background(), client, device, framePackage); err == nil {
		t.Fatal("ambiguous first create unexpectedly succeeded")
	}
	if _, err := uploadFramePackage(context.Background(), client, device, framePackage); err != nil {
		t.Fatalf("same-target retry did not accept 412: %v", err)
	}
	mutex.Lock()
	defer mutex.Unlock()
	if len(postNames) != 2 || postNames[0] != postNames[1] {
		t.Fatalf("retry changed target: %#v", postNames)
	}
}

func readMultipartUpload(request *http.Request) ([]byte, string, error) {
	_, parameters, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || parameters["boundary"] == "" {
		return nil, "", errors.New("multipart upload has no boundary")
	}
	reader := multipart.NewReader(request.Body, parameters["boundary"])
	metadata, err := reader.NextPart()
	if err != nil {
		return nil, "", err
	}
	if _, err := io.Copy(io.Discard, metadata); err != nil {
		return nil, "", err
	}
	media, err := reader.NextPart()
	if err != nil {
		return nil, "", err
	}
	payload, err := io.ReadAll(media)
	return payload, media.Header.Get("Content-Type"), err
}

func newTestStorageClient(t *testing.T, endpoint string) *storage.Client {
	t.Helper()
	client, err := storage.NewClient(
		context.Background(),
		option.WithEndpoint(endpoint),
		option.WithoutAuthentication(),
	)
	if err != nil {
		t.Fatalf("storage.NewClient() error = %v", err)
	}
	t.Cleanup(func() {
		_ = client.Close()
	})
	return client
}

func writeStorageError(writer http.ResponseWriter, status int) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_, _ = fmt.Fprintf(
		writer,
		`{"error":{"code":%d,"message":"test error"}}`,
		status,
	)
}

func testFramePackageUpload(t *testing.T) framePackageUpload {
	t.Helper()
	cache := newTestFramePackageCache(t, 15)
	if err := cache.addFrame(
		time.Date(2026, 9, 6, 11, 7, 3, 456999999, time.UTC),
		[]byte("jpeg-payload"),
	); err != nil {
		t.Fatal(err)
	}
	cache.advance(time.Date(2026, 9, 6, 11, 15, 0, 0, time.UTC))
	upload, ok := cache.completedUpload()
	if !ok {
		t.Fatal("test package did not close")
	}
	return upload
}
