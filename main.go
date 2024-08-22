package main

import (
	"archive/zip"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"strconv"

	// "log"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

type Connection struct {
	Name     string `json:"name"`
	Host     string `json:"host"`
	Port     string `json:"port"`
	Username string `json:"username"`
	Password string `json:"password"`
	Path     string `json:"path"`
}

type ConnectOptions struct {
	Host       string
	Port       string
	Username   string
	Password   string
	PrivateKey string
}

type SessionInfo struct {
	Client *sftp.Client
	LastUsed time.Time
}

type DownloadInfo struct {
	Created time.Time
	Paths   []string
	IsZip   bool
	Handler func(c *gin.Context)
}

var (
	connections     map[string]Connection
	sessions        sync.Map
	sessionActivity sync.Map
	rawDownloads    sync.Map
	upgrader        = websocket.Upgrader{
		ReadBufferSize:  1024 * 1024 * 4,
		WriteBufferSize: 1024 * 1024 * 4,
	}
)

func main() {
    router := gin.Default()

    // Serve static files
    router.Static("/static", "./web")

    // API routes
    api := router.Group("/api")
    {
        api.GET("/connections", getConnections)
        api.GET("/setActiveConnection/:id", setActiveConnection)

        sftp := api.Group("/sftp")
        {
            sftp.GET("/key", getSFTPKey)
            sftp.GET("/directories/list", listDirectories)
            sftp.GET("/directories/search", searchDirectories)
            sftp.POST("/directories/create", createDirectory)
            sftp.DELETE("/directories/delete", deleteDirectory)
            sftp.GET("/files/exists", fileExists)
            sftp.POST("/files/create", createFile)
            sftp.PUT("/files/append", appendFile)
            sftp.DELETE("/files/delete", deleteFile)
            sftp.PUT("/files/move", moveFile)
            sftp.PUT("/files/copy", copyFile)
            sftp.PUT("/files/chmod", chmodFile)
            sftp.GET("/files/stat", statFile)
            sftp.GET("/files/get/single", getSingleFile)
            sftp.GET("/files/get/single/url", getSingleFileURL)
            sftp.GET("/files/get/multi/url", getMultiFileURL)
        }
    }

    router.GET("/dl/:id", handleDownload)

    // Serve index.html for the root path
    router.GET("/", func(c *gin.Context) {
        c.File("./web/index.html")
    })

    // Start server
    router.Run(":8080")
}
	//

func getConnections(c *gin.Context) {
	c.JSON(http.StatusOK, connections)
}

func setActiveConnection(c *gin.Context) {
	id := c.Param("id")
	if id == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Missing id"})
		return
	}

	connection, ok := connections[id]
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "Connection not found"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"connection": connection})
}

func getSFTPKey(c *gin.Context) {
	key := generateRandomKey()
	c.JSON(http.StatusOK, gin.H{"key": key})
}

func listDirectories(c *gin.Context) {
	client, err := getSessionFromContext(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	path := normalizeRemotePath(c.Query("path"))
	if path == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Missing path"})
		return
	}

	files, err := client.ReadDir(path)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	var fileList []map[string]interface{}
	for _, file := range files {
		fileList = append(fileList, map[string]interface{}{
			"name": file.Name(),
			"type": file.Mode().String(),
			"size": file.Size(),
		})
	}

	c.JSON(http.StatusOK, gin.H{"list": fileList})
}

func searchDirectories(c *gin.Context) {
	// Upgrade HTTP connection to WebSocket
	ws, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		log.Printf("WebSocket upgrade failed: %v", err)
		return
	}
	defer ws.Close()

	// Get SFTP client
	client, err := getSessionFromContext(c)
	if err != nil {
		ws.WriteJSON(map[string]interface{}{"error": err.Error()})
		return
	}

	// Get search parameters
	path := normalizeRemotePath(c.Query("path"))
	query := strings.ToLower(c.Query("query"))

	if path == "" || query == "" {
		ws.WriteJSON(map[string]interface{}{"error": "Missing path or query"})
		return
	}

	// Start recursive search
	go searchRecursive(ws, client, path, query)
}


func searchRecursive(ws *websocket.Conn, client *sftp.Client, path, query string) {
	// Send scanning status
	ws.WriteJSON(map[string]interface{}{
		"status": "scanning",
		"path":   path,
	})

	// Read directory contents
	files, err := client.ReadDir(path)
	if err != nil {
		ws.WriteJSON(map[string]interface{}{"error": fmt.Sprintf("Failed to read directory %s: %v", path, err)})
		return
	}

	matchedFiles := []map[string]interface{}{}

	for _, file := range files {
		fullPath := filepath.Join(path, file.Name())

		// Check if the file name matches the query
		if strings.Contains(strings.ToLower(file.Name()), query) {
			matchedFiles = append(matchedFiles, map[string]interface{}{
				"name": file.Name(),
				"path": fullPath,
				"size": file.Size(),
				"mode": file.Mode().String(),
				"type": file.Mode().IsDir(),
			})
		}

		// If it's a directory, search recursively
		if file.IsDir() {
			searchRecursive(ws, client, fullPath, query)
		}

		// Send matched files periodically
		if len(matchedFiles) >= 10 {
			ws.WriteJSON(map[string]interface{}{
				"status": "results",
				"files":  matchedFiles,
			})
			matchedFiles = []map[string]interface{}{}
		}
	}

	// Send any remaining matched files
	if len(matchedFiles) > 0 {
		ws.WriteJSON(map[string]interface{}{
			"status": "results",
			"files":  matchedFiles,
		})
	}
}

func createDirectory(c *gin.Context) {
	client, err := getSessionFromContext(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	path := normalizeRemotePath(c.Query("path"))
	if path == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Missing path"})
		return
	}

	err = client.MkdirAll(path)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"success": true})
}

func deleteDirectory(c *gin.Context) {
	client, err := getSessionFromContext(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	path := normalizeRemotePath(c.Query("path"))
	if path == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Missing path"})
		return
	}

	err = client.RemoveDirectory(path)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"success": true})
}

func fileExists(c *gin.Context) {
	client, err := getSessionFromContext(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	path := normalizeRemotePath(c.Query("path"))
	if path == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Missing path"})
		return
	}

	info, err := client.Stat(path)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{"exists": false})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"exists": true,
		"type":   info.Mode().String(),
	})
}

func createFile(c *gin.Context) {
	client, err := getSessionFromContext(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	path := normalizeRemotePath(c.Query("path"))
	if path == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Missing path"})
		return
	}

	file, err := client.Create(path)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	defer file.Close()

	_, err = io.Copy(file, c.Request.Body)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"success": true})
}

func appendFile(c *gin.Context) {
	client, err := getSessionFromContext(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	path := normalizeRemotePath(c.Query("path"))
	if path == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Missing path"})
		return
	}

	file, err := client.OpenFile(path, os.O_APPEND|os.O_WRONLY)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	defer file.Close()

	_, err = io.Copy(file, c.Request.Body)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"success": true})
}

func deleteFile(c *gin.Context) {
	client, err := getSessionFromContext(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	path := normalizeRemotePath(c.Query("path"))
	if path == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Missing path"})
		return
	}

	err = client.Remove(path)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"success": true})
}

func moveFile(c *gin.Context) {
	client, err := getSessionFromContext(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	oldPath := normalizeRemotePath(c.Query("pathOld"))
	newPath := normalizeRemotePath(c.Query("pathNew"))
	if oldPath == "" || newPath == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Missing path"})
		return
	}

	err = client.Rename(oldPath, newPath)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"success": true})
}

func copyFile(c *gin.Context) {
	client, err := getSessionFromContext(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	srcPath := normalizeRemotePath(c.Query("pathSrc"))
	destPath := normalizeRemotePath(c.Query("pathDest"))
	if srcPath == "" || destPath == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Missing path"})
		return
	}

	srcFile, err := client.Open(srcPath)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	defer srcFile.Close()

	destFile, err := client.Create(destPath)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	defer destFile.Close()

	_, err = io.Copy(destFile, srcFile)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"success": true})
}

func chmodFile(c *gin.Context) {
	client, err := getSessionFromContext(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	path := normalizeRemotePath(c.Query("path"))
	mode := c.Query("mode")
	if path == "" || mode == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Missing path or mode"})
		return
	}

	modeInt, err := parseMode(mode)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid mode"})
		return
	}

	err = client.Chmod(path, modeInt)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"success": true})
}

func statFile(c *gin.Context) {
	client, err := getSessionFromContext(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	path := normalizeRemotePath(c.Query("path"))
	if path == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Missing path"})
		return
	}

	info, err := client.Stat(path)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"name":  info.Name(),
		"size":  info.Size(),
		"mode":  info.Mode().String(),
		"isDir": info.IsDir(),
	})
}

func getSingleFile(c *gin.Context) {
	client, err := getSessionFromContext(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	path := normalizeRemotePath(c.Query("path"))
	if path == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Missing path"})
		return
	}

	file, err := client.Open(path)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	defer file.Close()

	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=%s", filepath.Base(path)))
	c.Header("Content-Type", mime.TypeByExtension(filepath.Ext(path)))

	_, err = io.Copy(c.Writer, file)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
}

func getSingleFileURL(c *gin.Context) {
	path := normalizeRemotePath(c.Query("path"))
	if path == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Missing path"})
		return
	}

	id := generateRandomKey()
	downloadURL := fmt.Sprintf("https://%s/dl/%s", c.Request.Host, id)

	rawDownloads.Store(id, DownloadInfo{
		Created: time.Now(),
		Paths:   []string{path},
		IsZip:   false,
		Handler: func(c *gin.Context) {
			getSingleFile(c)
		},
	})

	c.JSON(http.StatusOK, gin.H{"download_url": downloadURL})
}

func getMultiFileURL(c *gin.Context) {
	pathsJSON := c.Query("paths")
	var paths []string
	err := json.Unmarshal([]byte(pathsJSON), &paths)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid paths"})
		return
	}

	id := generateRandomKey()
	downloadURL := fmt.Sprintf("https://%s/dl/%s", c.Request.Host, id)

	rawDownloads.Store(id, DownloadInfo{
		Created: time.Now(),
		Paths:   paths,
		IsZip:   true,
		Handler: func(c *gin.Context) {
			getMultiFile(c, paths)
		},
	})

	c.JSON(http.StatusOK, gin.H{"download_url": downloadURL})
}

func handleDownload(c *gin.Context) {
	id := c.Param("id")
	downloadInfo, ok := rawDownloads.Load(id)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "Download not found"})
		return
	}

	info := downloadInfo.(DownloadInfo)
	info.Handler(c)
}

func getMultiFile(c *gin.Context, paths []string) {
	client, err := getSessionFromContext(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	c.Header("Content-Disposition", "attachment; filename=files.zip")
	c.Header("Content-Type", "application/zip")

	zipWriter := zip.NewWriter(c.Writer)
	defer zipWriter.Close()

	for _, path := range paths {
		err := addFileToZip(client, zipWriter, path)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
	}
}

func addFileToZip(client *sftp.Client, zipWriter *zip.Writer, path string) error {
	file, err := client.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return err
	}

	header, err := zip.FileInfoHeader(info)
	if err != nil {
		return err
	}

	header.Name = path
	header.Method = zip.Deflate

	writer, err := zipWriter.CreateHeader(header)
	if err != nil {
		return err
	}

	_, err = io.Copy(writer, file)
	return err
}

func getSessionFromContext(c *gin.Context) (*sftp.Client, error) {
	opts := ConnectOptions{
		Host:       c.GetHeader("sftp-host"),
		Port:       c.GetHeader("sftp-port"),
		Username:   c.GetHeader("sftp-username"),
		Password:   c.GetHeader("sftp-password"),
		PrivateKey: c.GetHeader("sftp-key"),
	}

	if opts.Host == "" || opts.Username == "" || (opts.Password == "" && opts.PrivateKey == "") {
		return nil, fmt.Errorf("missing connection details")
	}

	return getSession(opts)
}

func getSession(opts ConnectOptions) (*sftp.Client, error) {
	hash := getObjectHash(opts)

	if session, ok := sessions.Load(hash); ok {
		sessionInfo := session.(SessionInfo)
		sessionInfo.LastUsed = time.Now()
		sessions.Store(hash, sessionInfo)
		return sessionInfo.Client, nil
	}

	config := &ssh.ClientConfig{
		User: opts.Username,
		Auth: []ssh.AuthMethod{},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	}

	if opts.Password != "" {
		config.Auth = append(config.Auth, ssh.Password(opts.Password))
	}

	if opts.PrivateKey != "" {
		key, err := ssh.ParsePrivateKey([]byte(opts.PrivateKey))
		if err != nil {
			return nil, err
		}
		config.Auth = append(config.Auth, ssh.PublicKeys(key))
	}

	conn, err := ssh.Dial("tcp", fmt.Sprintf("%s:%s", opts.Host, opts.Port), config)
	if err != nil {
		return nil, err
	}

	client, err := sftp.NewClient(conn)
	if err != nil {
		return nil, err
	}

	sessionInfo := SessionInfo{
		Client:   client,
		LastUsed: time.Now(),
	}
	sessions.Store(hash, sessionInfo)

	return client, nil
}

func normalizeRemotePath(remotePath string) string {
	remotePath = filepath.ToSlash(filepath.Clean(remotePath))
	split := strings.Split(remotePath, "/")
	var filtered []string
	for _, s := range split {
		if s != "" {
			filtered = append(filtered, s)
		}
	}
	return "/" + strings.Join(filtered, "/")
}

func getObjectHash(obj interface{}) string {
	jsonBytes, _ := json.Marshal(obj)
	hash := sha256.Sum256(jsonBytes)
	return hex.EncodeToString(hash[:])
}

func generateRandomKey() string {
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(bytes)
}

func parseMode(mode string) (os.FileMode, error) {
	modeInt, err := strconv.ParseUint(mode, 8, 32)
	if err != nil {
		return 0, err
	}
	return os.FileMode(modeInt), nil
}

func cleanupSessions() {
	sessions.Range(func(key, value interface{}) bool {
		sessionInfo := value.(SessionInfo)
		if time.Since(sessionInfo.LastUsed) > 5*time.Minute {
			sessionInfo.Client.Close()
			sessions.Delete(key)
		}
		return true
	})
}

func cleanupDownloads() {
	rawDownloads.Range(func(key, value interface{}) bool {
		downloadInfo := value.(DownloadInfo)
		if time.Since(downloadInfo.Created) > 12*time.Hour {
			rawDownloads.Delete(key)
		}
		return true
	})
}

func init() {
	// Initialize connections
	connections = map[string]Connection{
		"1718263336899": {
			Name:     "New Connection",
			Host:     "103.165.95.123",
			Port:     "2233",
			Username: "user2",
			Password: "pass2user",
			Path:     "/home/user2",
		},
	}

	// Start cleanup goroutine
	go func() {
		for {
			time.Sleep(30 * time.Second)
			cleanupSessions()
			cleanupDownloads()
		}
	}()
}