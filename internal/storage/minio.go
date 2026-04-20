package storage

import (
	"context"
	"log"
	"net/url"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

type MinioUploader struct {
	client *minio.Client
	bucket string
}

func NewMinioUploader(endpoint, accessKey, secretKey, bucket string) (*MinioUploader, error) {
	parsedUrl, err := url.Parse(endpoint)
	var host string
	var useSSL bool

	if err == nil && parsedUrl.Host != "" {
		host = parsedUrl.Host
		useSSL = parsedUrl.Scheme == "https"
	} else {
		host = strings.TrimPrefix(endpoint, "http://")
		host = strings.TrimPrefix(host, "https://")
		useSSL = strings.HasPrefix(endpoint, "https://")
	}

	client, err := minio.New(host, &minio.Options{
		Creds:  credentials.NewStaticV4(accessKey, secretKey, ""),
		Secure: useSSL,
	})
	if err != nil {
		return nil, err
	}

	ctx := context.Background()
	exists, err := client.BucketExists(ctx, bucket)
	if err != nil {
		return nil, err
	}
	if !exists {
		err = client.MakeBucket(ctx, bucket, minio.MakeBucketOptions{})
		if err != nil {
			return nil, err
		}
		log.Printf("Storage: Bucket '%s' created", bucket)
	} else {
		log.Printf("Storage: Bucket '%s' already exists", bucket)
	}

	return &MinioUploader{
		client: client,
		bucket: bucket,
	}, nil
}

// загружает файл на s3 с правильным content-type
func (u *MinioUploader) UploadFile(ctx context.Context, filePath, objectName string) error {
	contentType := "application/octet-stream"
	if strings.HasSuffix(objectName, ".m3u8") {
		contentType = "application/x-mpegURL"
	} else if strings.HasSuffix(objectName, ".mp4") || strings.HasSuffix(objectName, ".m4s") {
		contentType = "video/mp4"
	}

	_, err := u.client.FPutObject(ctx, u.bucket, objectName, filePath, minio.PutObjectOptions{
		ContentType: contentType,
	})
	return err
}
