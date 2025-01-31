package main

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"strings"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/google/uuid"
)

func (cfg *apiConfig) handlerUploadVideo(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, int64(10<<30))

	videoIDString := r.PathValue("videoID")
	videoID, err := uuid.Parse(videoIDString)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "missing video id parameter", err)
		return
	}

	token, err := auth.GetBearerToken(r.Header)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "unauthorized", err)
		return
	}
	userID, err := auth.ValidateJWT(token, cfg.jwtSecret)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "unauthorized", err)
		return
	}

	videoMetadata, err := cfg.db.GetVideo(videoID)
	if err != nil {
		respondWithError(w, http.StatusNotFound, "not found", err)
		return
	}
	if videoMetadata.UserID != userID {
		respondWithError(w, http.StatusUnauthorized, "unauthorized", err)
		return
	}

	file, header, err := r.FormFile("video")
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Could not get video file", err)
		return
	}
	defer file.Close()

	mediaType, _, err := mime.ParseMediaType(header.Header.Get("content-type"))
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Unable to parse mimetype", err)
		return
	}
	if mediaType != "video/mp4" {
		respondWithError(w, http.StatusBadRequest, "only video/mp4 mimetype accepted", err)
		return
	}

	tmpFile, err := os.CreateTemp("", "video-upload.mp4")
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "unable to create temp file", err)
		return
	}
	defer os.Remove(tmpFile.Name())
	defer tmpFile.Close()

	_, err = io.Copy(tmpFile, file)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "unable to copy file", err)
		return
	}

	_, err = tmpFile.Seek(0, io.SeekStart)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "unable to find file start", err)
		return
	}

	aspectRatio, err := getVideoAspectRatio(tmpFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "cannot get video aspect ratio", err)
		return
	}
	var prefix string
	switch aspectRatio {
	case "16:9":
		prefix = "landscape"
	case "9:16":
		prefix = "portrait"
	case "other":
		prefix = "other"
	}

    processedPath, err := processVideoForFastStart(tmpFile.Name())
    if err != nil {
        respondWithError(w, http.StatusInternalServerError, "unable to process video", err)
        return
    }
    processedFile, err := os.Open(processedPath)
    if err != nil {
        respondWithError(w, http.StatusInternalServerError, "error reading processed file", err)
        return
    }

	extension := strings.Split(mediaType, "/")[1]
	randBuf := make([]byte, 32)
	_, err = rand.Read(randBuf)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "cannot create random buf", err)
		return
	}
	randBufBase64 := base64.RawURLEncoding.EncodeToString(randBuf)
	filename := prefix + "/" + randBufBase64 + "." + extension

	params := s3.PutObjectInput{
		Bucket:      &cfg.s3Bucket,
		Key:         &filename,
		Body:        processedFile,
		ContentType: &mediaType,
	}
	_, err = cfg.s3Client.PutObject(r.Context(), &params)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "unable to write to s3", err)
		return
	}

	url := fmt.Sprintf("%s/%s", cfg.s3CfDistribution, filename)
	videoMetadata.VideoURL = &url
	err = cfg.db.UpdateVideo(videoMetadata)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "unable to update video metadata", err)
		return
	}

	respondWithJSON(w, http.StatusOK, videoMetadata)
}

func getVideoAspectRatio(filePath string) (string, error) {
	cmd := exec.Command("ffprobe", "-v", "error", "-print_format", "json", "-show_streams", filePath)
	buf := bytes.Buffer{}
	cmd.Stdout = &buf
	err := cmd.Run()
	if err != nil {
		return "", err
	}

	data := struct {
		Streams []struct {
			Height float64 `json:"height"`
			Width  float64 `json:"width"`
        } `json:"streams"`
	}{}
	err = json.Unmarshal(buf.Bytes(), &data)
	// height / width > 1 < 2 then 16:9 else < 1 then 9:16 else > 2 then other
	// This is not the ideal way to determine aspect ration, but for this demo it is sufficient
    if len(data.Streams) < 1 {
        return "", errors.New("Missing video stream data")
    }
	result := data.Streams[0].Width / data.Streams[0].Height
	switch {
	case result < 1.0:
		return "9:16", nil
	case result > 1.0 && result < 2.0:
		return "16:9", nil
	default:
		return "other", nil
	}
}

func processVideoForFastStart(filePath string) (string, error) {
    outputPath := filePath + ".processing"
    cmd := exec.Command(
        "ffmpeg", "-i", filePath,
        "-c", "copy", "-movflags",
        "faststart", "-f", "mp4",
        outputPath,
    )
    err := cmd.Run()
    if err != nil {
        return "", err
    }

    return outputPath, nil
}
