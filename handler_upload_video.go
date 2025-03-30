package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/database"
	"github.com/google/uuid"
)

func (cfg *apiConfig) handlerUploadVideo(w http.ResponseWriter, r *http.Request) {
	videoIDString := r.PathValue("videoID")
	videoID, err := uuid.Parse(videoIDString)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid ID", err)
		return
	}

	token, err := auth.GetBearerToken(r.Header)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't find JWT", err)
		return
	}

	userID, err := auth.ValidateJWT(token, cfg.jwtSecret)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't validate JWT", err)
		return
	}

	fmt.Println("uploading video", videoID, "by user", userID)

	// TODO: implement the upload here
	r.ParseMultipartForm(10 << 30) // 1 GB limit
	file, file_header, err := r.FormFile("video")
	defer file.Close()

	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Couldn't parse form", err)
		return
	}
	mediaType, _, err := mime.ParseMediaType(file_header.Header.Get("Content-Type"))
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Couldn't parse media type", err)
		return
	}
	if mediaType != "video/mp4" {
		respondWithError(w, http.StatusBadRequest, "Invalid media type", nil)
		return
	}
	file_ext := strings.Split(mediaType, "/")[1]

	temp_file, err := os.CreateTemp("", "tubely-upload.mp4")
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't create temp file", err)
		return
	}

	defer os.Remove(temp_file.Name())
	defer temp_file.Close()
	_, err = io.Copy(temp_file, file)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't copy file", err)
		return
	}
	// reset file pointer
	temp_file.Seek(0, io.SeekStart)

	rand_bytes := make([]byte, 32)

	_, err = rand.Read(rand_bytes)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't generate random bytes", err)
		return
	}

	// Process the video for fast start
	processedFilePath, err := processVideoForFastStart(temp_file.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't process video", err)
		return
	}
	defer os.Remove(processedFilePath)

	// open processed file
	processedFile, err := os.Open(processedFilePath)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't open processed file", err)
		return
	}

	aspect_prefix, err := getVideoAspectRatio(temp_file.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't get video aspect ratio", err)
		return
	}

	fn := base64.RawURLEncoding.EncodeToString(rand_bytes)

	s3Key := fmt.Sprintf("%s/%s.%s", aspect_prefix, fn, file_ext)

	dataUrl := fmt.Sprintf("%s,%s", cfg.s3Bucket, s3Key)

	_, err = cfg.s3Client.PutObject(r.Context(), &s3.PutObjectInput{
		Bucket:      aws.String(cfg.s3Bucket),
		Key:         aws.String(s3Key),
		Body:        processedFile,
		ContentType: aws.String(mediaType),
	})
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't upload file", err)
		return
	}
	fmt.Println("Uploaded video to S3")

	video, err := cfg.db.GetVideo(videoID)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't get video", err)
		return
	}

	if video.UserID != userID {
		respondWithError(w, http.StatusForbidden, "You don't have permission to upload this video", nil)
		return
	}

	video.VideoURL = &dataUrl

	video.UpdatedAt = time.Now()
	err = cfg.db.UpdateVideo(video)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't update video", err)
		return
	}

	video, err = cfg.dbVideoToSignedVideo(video)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't convert video to signed URL", err)
		return
	}

	respondWithJSON(w, http.StatusOK, video)
}

type Stream struct {
	Width  int `json:"width"`
	Height int `json:"height"`
}

type VideoInfo struct {
	Streams []Stream `json:"streams"`
}

func getVideoAspectRatio(filePath string) (string, error) {
	cmd := exec.Command("ffprobe", "-v", "error", "-print_format", "json", "-show_streams", filePath)
	buffer := bytes.NewBuffer(nil)
	cmd.Stdout = buffer

	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		log.Println("Error:", err)
		log.Println("Stderr:", stderr.String())
		return "", err
	}

	var videoInfo VideoInfo
	err := json.Unmarshal(buffer.Bytes(), &videoInfo)
	if err != nil {
		return "", err
	}

	var width, height int

	if len(videoInfo.Streams) > 0 {
		width = videoInfo.Streams[0].Width
		height = videoInfo.Streams[0].Height
		// Use width and height as needed
	} else {
		return "", fmt.Errorf("no video streams found")
	}

	if width/height == 16/9 {
		return "landscape", nil
	} else if height/width == 16/9 {
		return "portrait", nil
	} else {
		return "other", nil
	}

}

func processVideoForFastStart(filePath string) (string, error) {
	outputFilePath := filePath + ".processing"
	cmd := exec.Command("ffmpeg", "-i", filePath, "-c", "copy", "-movflags", "faststart", "-f", "mp4", outputFilePath)

	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		log.Println("Error:", err)
		log.Println("Stderr:", stderr.String())
		return "", err
	}

	return outputFilePath, nil
}

func generatePresignedURL(s3Client *s3.Client, bucket, key string, expireTime time.Duration) (string, error) {

	presignClient := s3.NewPresignClient(s3Client)

	r, err := presignClient.PresignGetObject(context.TODO(), &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	}, s3.WithPresignExpires(expireTime))

	if err != nil {
		return "", fmt.Errorf("failed to presign: %w", err)
	}

	return r.URL, nil
}

func (cfg *apiConfig) dbVideoToSignedVideo(video database.Video) (database.Video, error) {
	if video.VideoURL == nil {
		return database.Video{}, fmt.Errorf("video URL is nil")
	}

	url_split := strings.Split(*video.VideoURL, ",")
	if len(url_split) != 2 {
		return database.Video{}, fmt.Errorf("invalid video URL format: %s", video.VideoURL)
	}

	bucket, key := url_split[0], url_split[1]

	// Make sure cfg.s3Client is not nil here
	if cfg.s3Client == nil {
		return database.Video{}, fmt.Errorf("s3 client is nil")
	}

	presignedURL, err := generatePresignedURL(cfg.s3Client, bucket, key, 15*time.Minute)
	if err != nil {
		return video, fmt.Errorf("failed to generate presigned URL: %w", err)
	}
	video.VideoURL = &presignedURL
	return video, nil
}
