package main

import (
	"context"
	"fmt"
	"github.com/getsentry/sentry-go"
	"github.com/jackc/pgx/v4/pgxpool"
	"io"
	"log"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	defaultCacheDir = "/cache"
)

var (
	dsn        = os.Getenv("DSN")
	cacheDir   string
	logger     *log.Logger
	db         *pgxpool.Pool
	httpClient = &http.Client{}
)

func init() {
	err := sentry.Init(sentry.ClientOptions{
		Dsn:              "<SENTRY_DSN>",
		TracesSampleRate: 0.8,
	})
	if err != nil {
		log.Fatalf("sentry.Init failed: %s", err)
	}
	defer sentry.Flush(2 * time.Second)

	cacheDir = os.Getenv("CACHE_DIR")
	if cacheDir == "" {
		cacheDir = defaultCacheDir
	}

	logger = log.New(os.Stdout, "[repoproxy] ", log.LstdFlags|log.Lmicroseconds)
	var errDB error
	db, errDB = pgxpool.Connect(context.Background(), dsn)
	if errDB != nil {
		logger.Fatalf("Failed to initiate DB %s", errDB)
	}
}

func getRepoMap(repo string) (string, error) {
	var baseURL string
	err := db.QueryRow(context.Background(), "SELECT baseurl FROM repomap WHERE reponame = $1", repo).Scan(&baseURL)
	if err != nil {
		return "", fmt.Errorf("failed to query repomap: %w", err)
	}
	return baseURL, nil
}

func itemInCache(repo, itemPath, lastMod string, fileSize int64, etag string) (bool, error) {
	var cachedLastMod string
	var cachedFileSize int64
	var cachedEtag string

	err := db.QueryRow(context.Background(), "SELECT lastmodified, filesize, etag FROM cacheitem WHERE reponame = $1 AND pathname = $2", repo, itemPath).Scan(&cachedLastMod, &cachedFileSize, &cachedEtag)
	if err != nil {
		return false, nil
	}
	if cachedLastMod == lastMod && cachedFileSize == fileSize && cachedEtag == etag {
		return true, nil
	} else {
		return false, nil
	}
}

func updateCache(ctx context.Context, repo, itemPath, lastMod string, fileSize int64, etag string) error {
	var count int
	err := db.QueryRow(ctx, "SELECT COUNT(*) FROM cacheitem WHERE reponame = $1 AND pathname = $2", repo, itemPath).Scan(&count)
	if err != nil {
		return fmt.Errorf("failed to check cacheitem count: %w", err)
	}

	var sqlStmt string
	tx, err := db.Begin(ctx)
	if err != nil {
		logger.Println("ERR_TXSTART", err)
		return err
	}
	defer tx.Rollback(ctx)
	if count == 0 {
		sqlStmt = "INSERT INTO cacheitem (reponame, pathname, lastmodified, filesize, etag, updatedate) VALUES ($1, $2, $3, $4, $5, NOW())"
		_, err = tx.Exec(context.Background(), sqlStmt, repo, itemPath, lastMod, fileSize, etag)
	} else {
		sqlStmt = "UPDATE cacheitem SET lastmodified = $1, filesize = $2, etag = $3, updatedate = NOW() WHERE reponame = $4 AND pathname = $5"
		_, err = tx.Exec(context.Background(), sqlStmt, lastMod, fileSize, etag, repo, itemPath)
	}
	err = tx.Commit(ctx)
	if err != nil {
		return fmt.Errorf("failed to update cacheitem: %w", err)
	}

	return nil
}

func prepareCacheDir(repo, itemPath string) (string, error) {
	dirName := filepath.Dir(itemPath)
	cacheDirFullPath := filepath.Join(cacheDir, repo, dirName)
	err := os.MkdirAll(cacheDirFullPath, 0755)
	if err != nil {
		return "", fmt.Errorf("failed to create cache directory: %w", err)
	}
	cachePath := filepath.Join(cacheDir, repo, itemPath)
	return cachePath, nil
}

func getClientIP(r *http.Request) string {
	forwardedFor := r.Header.Get("X-Forwarded-For")
	if forwardedFor != "" {
		ips := strings.Split(forwardedFor, ",")
		if len(ips) > 0 {
			return strings.TrimSpace(ips[0])
		}
	}
	ip, err := netip.ParseAddrPort(r.RemoteAddr)
	if err == nil {
		return ip.String()
	}
	return r.RemoteAddr
}

func mainHandler(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	repoUrl := strings.TrimPrefix(r.URL.Path, "/r/")
	path := strings.SplitN(repoUrl, "/", 2)
	repoName := path[0]
	rest := path[1]
	remoteBase, err := getRepoMap(repoName)
	if err != nil {
		logger.Println("ERR_GETREPOMAP", err)
		sentry.CaptureException(err)
		http.Error(w, "NOT FOUND", http.StatusNotFound)
		return
	}
	remoteBase = strings.TrimSuffix(remoteBase, "/")
	targetUrl := fmt.Sprintf("%s/%s", remoteBase, rest)
	respHead, err := httpClient.Head(targetUrl)
	if err != nil {
		logger.Println("ERR_HEAD", err)
		http.Error(w, "INTERNAL SERVER ERROR", http.StatusInternalServerError)
		return
	}
	defer respHead.Body.Close()
	lastMod := respHead.Header.Get("Last-Modified")
	contentLengthRaw := respHead.Header.Get("Content-Length")
	contentLength, _ := strconv.ParseInt(contentLengthRaw, 10, 64)
	etag := respHead.Header.Get("Etag")
	mimeType := respHead.Header.Get("Content-Type")
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}

	cachePath, err := prepareCacheDir(repoName, rest)
	if err != nil {
		logger.Println("ERR_PREPARECACHEPATH", err)
	}

	clientAddress := getClientIP(r)
	inCache, _ := itemInCache(repoName, rest, lastMod, contentLength, etag)

	if _, err := os.Stat(cachePath); !os.IsNotExist(err) && inCache {
		http.ServeFile(w, r, cachePath)
		return
	}

	logger.Println("INFO_CACHE_MISS", clientAddress, repoName, rest)

	respGet, err := httpClient.Get(targetUrl)
	if err != nil {
		logger.Println("ERR_GET", err)
		http.Error(w, "FETCH ERROR", http.StatusInternalServerError)
		return
	}
	defer respGet.Body.Close()

	if respGet.StatusCode >= http.StatusBadRequest {
		http.Error(w, fmt.Sprintf("UPSTREAM ERROR %s", respGet.Status), respGet.StatusCode)
		return
	}

	file, err := os.Create(cachePath)
	if err != nil {
		logger.Println("ERR_CREATECACHEPATH", err)
		http.Error(w, "CACHE CREATE ERROR", http.StatusInternalServerError)
		return
	}
	defer file.Close()

	logger.Println("DBG_HEAD", mimeType, contentLength)
	respContentLastMod := respGet.Header.Get("Last-Modified")
	respContentEtag := respGet.Header.Get("ETag")
	respContentLengthRaw := respGet.Header.Get("Content-Length")
	respContentLength, _ := strconv.ParseInt(respContentLengthRaw, 10, 64)
	logger.Println("DBG_RESP", respContentLength, respContentLastMod)
	w.Header().Set("Content-Type", mimeType)
	_, err = io.Copy(io.MultiWriter(w, file), respGet.Body)
	if err != nil {
		logger.Println("ERR_STREAM", err)
		return
	}

	if respContentLengthRaw == "" {
		respContentLength = contentLength
	}
	respContentEtag = strings.TrimPrefix(respContentEtag, "W/")
	logger.Println("DBG_CACHEPAYLOAD", respContentLastMod, respContentLength, respContentEtag)
	err = updateCache(ctx, repoName, rest, respContentLastMod, respContentLength, respContentEtag)
	if err != nil {
		logger.Println("ERR_UPDATECACHE", err)
	}

}

func main() {
	http.HandleFunc("/r/", mainHandler)

	port := os.Getenv("PORT")
	if port == "" {
		port = "5000"
	}
	logger.Printf("Server listening on port %s", port)
	if err := http.ListenAndServe(":"+port, nil); err != nil {
		logger.Fatalf("Failed to run server: %v", err)
	}
}
