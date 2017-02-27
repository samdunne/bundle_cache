package main

import (
	"bytes"
	"crypto/sha1"
	"fmt"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/s3/s3manager"
	"github.com/jessevdk/go-flags"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

const VERSION = "0.3.0"

const (
	ERR_OK             = 0
	ERR_WRONG_USAGE    = 2
	ERR_NO_CREDENTIALS = 3
	ERR_NO_BUNDLE      = 4
	ERR_NO_GEMLOCK     = 5
)

var options struct {
	Prefix        string `long:"prefix"     description:"Custom archive filename (default: current dir)"`
	Path          string `long:"path"       description:"Path to directory with .bundle (default: current)"`
	AccessKey     string `long:"access-key" description:"AmazonS3 Access key"`
	SecretKey     string `long:"secret-key" description:"AmazonS3 Secret key"`
	Bucket        string `long:"bucket"     description:"AmazonS3 Bucket name"`
	Region        string `long:"region"      description:"AWS Region"`
	BundlePath    string
	LockFilePath  string
	CacheFilePath string
	ArchiveName   string
	ArchivePath   string
}

func terminate(message string, exit_code int) {
	fmt.Fprintln(os.Stderr, message)
	os.Exit(exit_code)
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func sh(command string) (string, error) {
	var output bytes.Buffer

	cmd := exec.Command("bash", "-c", command)

	cmd.Stdout = &output
	cmd.Stderr = &output

	err := cmd.Run()
	return output.String(), err
}

func calculateChecksum(buffer string) string {
	h := sha1.New()
	io.WriteString(h, buffer)
	return fmt.Sprintf("%x", h.Sum(nil))
}

func extractArchive(filename string, path string) bool {
	cmd_mkdir := fmt.Sprintf("cd %s && mkdir .bundle", path)
	cmd_move := fmt.Sprintf("mv %s %s/.bundle/bundle_cache.tar.gz", filename, path)
	cmd_extract := fmt.Sprintf("cd %s/.bundle && tar -xzf ./bundle_cache.tar.gz", path)
	cmd_remove := fmt.Sprintf("rm %s/.bundle/bundle_cache.tar.gz", path)

	if _, err := sh(cmd_mkdir); err != nil {
		fmt.Println("Bundle directory '.bundle' already exists")
		return false
	}

	if _, err := sh(cmd_move); err != nil {
		fmt.Printf("Unable to move file: %s", err)
		return false
	}

	if out, err := sh(cmd_extract); err != nil {
		fmt.Println("Unable to extract:", out)
		return false
	}

	if _, err := sh(cmd_remove); err != nil {
		fmt.Println("Unable to remove archive")
		return false
	}

	return true
}

func envDefined(name string) bool {
	result := os.Getenv(name)
	return len(result) > 0
}

func checkS3Credentials() {
	if len(options.AccessKey) == 0 && envDefined("AWS_ACCESS_KEY") {
		options.AccessKey = os.Getenv("AWS_ACCESS_KEY")
	}

	if len(options.SecretKey) == 0 && envDefined("AWS_SECRET_KEY") {
		options.SecretKey = os.Getenv("AWS_SECRET_KEY")
	}

	if len(options.Bucket) == 0 && envDefined("S3_BUCKET") {
		options.Bucket = os.Getenv("S3_BUCKET")
	}

	if len(options.Region) == 0 && envDefined("AWS_DEFAULT_REGION") {
		options.Region = os.Getenv("AWS_DEFAULT_REGION")
	}

	if len(options.AccessKey) == 0 {
		terminate("Please provide S3 access key", ERR_NO_CREDENTIALS)
	}

	if len(options.SecretKey) == 0 {
		terminate("Please provide S3 secret key", ERR_NO_CREDENTIALS)
	}

	if len(options.Bucket) == 0 {
		terminate("Please provide S3 bucket name", ERR_NO_CREDENTIALS)
	}

	if len(options.Region) == 0 {
		terminate("Please provide S3 region name", ERR_NO_CREDENTIALS)
	}
}

func printUsage() {
	terminate("Usage: bundle_cache [download|upload]", ERR_WRONG_USAGE)
}

func upload(cfg *aws.Config) {
	if fileExists(options.CacheFilePath) {
		terminate("Your bundle is cached, skipping.", ERR_OK)
	}

	svc := s3.New(session.New(), cfg)

	if !fileExists(options.BundlePath) {
		terminate("Bundle path does not exist", ERR_NO_BUNDLE)
	}

	fmt.Println("Archiving...")
	cmd := fmt.Sprintf("cd %s && tar -czf %s .", options.BundlePath, options.ArchivePath)
	if _, err := sh(cmd); err != nil {
		terminate("Failed to make archive.", 1)
	}

	file, err := os.Open(options.ArchivePath)
	if err != nil {
		fmt.Printf("err opening file: %s", err)
	}
	defer file.Close()
	fileInfo, _ := file.Stat()
	size := fileInfo.Size()
	buffer := make([]byte, size) // read file content to buffer

	file.Read(buffer)
	fileBytes := bytes.NewReader(buffer)
	fileType := http.DetectContentType(buffer)

	fmt.Println("Uploading bundle to S3...")
	params := &s3.PutObjectInput{
		Bucket:        aws.String(options.Bucket),
		Key:           aws.String(options.ArchivePath),
		Body:          fileBytes,
		ContentLength: aws.Int64(size),
		ContentType:   aws.String(fileType),
	}

	_, err = svc.PutObject(params)
	if err != nil {
		fmt.Printf("bad response: %s", err)
	}

	fmt.Println("Done")
	os.Exit(0)
}

func download(cfg *aws.Config) {
	if fileExists(options.BundlePath) {
		terminate("Bundle path already exists, skipping.", 0)
	}

	file, err := os.Create(options.ArchivePath)
	if err != nil {
		fmt.Printf("err opening file: %s", err)
	}

	fmt.Println("Downloading bundle from S3...", options.ArchiveName)
	downloader := s3manager.NewDownloader(session.New(cfg))
	_, err = downloader.Download(file,
		&s3.GetObjectInput{
			Bucket: aws.String(options.Bucket),
			Key:    aws.String(options.ArchivePath),
		})

	if err != nil {
		fmt.Printf("bad response: %s", err)
	}

	/* Extract archive into bundle directory */
	fmt.Println("Extracting...")
	extractArchive(options.ArchivePath, options.Path)

	/* Create a temp file in path to indicate that bundle was cached */
	if !fileExists(options.CacheFilePath) {
		sh(fmt.Sprintf("touch %s", options.CacheFilePath))
	}

	fmt.Println("Done")
	os.Exit(0)
}

func getAction() string {
	new_args, err := flags.ParseArgs(&options, os.Args)

	if err != nil {
		fmt.Println(err)
		os.Exit(ERR_WRONG_USAGE)
	}

	args := new_args[1:]

	if len(args) != 1 {
		printUsage()
	}

	return args[0]
}

func setOptions() {
	if len(options.Path) == 0 {
		options.Path, _ = os.Getwd()
	}

	if len(options.Prefix) == 0 {
		options.Prefix = filepath.Base(options.Path)
	}

	options.BundlePath = fmt.Sprintf("%s/.bundle", options.Path)
	options.LockFilePath = fmt.Sprintf("%s/Gemfile.lock", options.Path)
	options.CacheFilePath = fmt.Sprintf("%s/.cache", options.BundlePath)
}

func setArchiveOptions() {
	lockfile, err := ioutil.ReadFile(options.LockFilePath)
	if err != nil {
		terminate("Unable to read Gemfile.lock", 1)
	}

	checksum := calculateChecksum(string(lockfile))

	options.ArchiveName = fmt.Sprintf("%s_%s_%s.tar.gz", options.Prefix, checksum, runtime.GOARCH)
	options.ArchivePath = fmt.Sprintf("/tmp/%s", options.ArchiveName)

	if fileExists(options.ArchivePath) {
		if os.Remove(options.ArchivePath) != nil {
			terminate("Failed to remove existing archive", 1)
		}
	}
}

func checkGemlockFile() {
	if !fileExists(options.LockFilePath) {
		message := fmt.Sprintf("%s does not exist", options.LockFilePath)
		terminate(message, ERR_NO_GEMLOCK)
	}
}

func main() {
	action := getAction()

	checkS3Credentials()

	token := ""

	creds := credentials.NewStaticCredentials(options.AccessKey, options.SecretKey, token)
	_, err := creds.Get()
	if err != nil {
		fmt.Printf("Bad credentials: %s", err)
	}

	cfg := aws.NewConfig().WithRegion(options.Region).WithCredentials(creds)

	setOptions()
	checkGemlockFile()
	setArchiveOptions()

	switch action {
	default:
		fmt.Println("Invalid command:", action)
		printUsage()
	case "upload":
		upload(cfg)
	case "download":
		download(cfg)
	}
}
