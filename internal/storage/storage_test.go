/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package storage

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestStorage(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Storage Suite")
}

var _ = Describe("Storage Factory", func() {
	Describe("NewStorage", func() {
		Context("with S3 storage type", func() {
			It("should create S3 storage", func() {
				config := StorageConfig{
					Type:      "s3",
					Endpoint:  "https://s3.amazonaws.com",
					Bucket:    "test-bucket",
					Region:    "us-east-1",
					AccessKey: "test-key",
					SecretKey: "test-secret",
				}

				storage, err := NewStorage(config)
				Expect(err).NotTo(HaveOccurred())
				Expect(storage).NotTo(BeNil())

				err = storage.Close()
				Expect(err).NotTo(HaveOccurred())
			})
		})

		Context("with MinIO storage type", func() {
			It("should create S3-compatible storage", func() {
				config := StorageConfig{
					Type:      "minio",
					Endpoint:  "https://minio.example.com",
					Bucket:    "test-bucket",
					AccessKey: "test-key",
					SecretKey: "test-secret",
				}

				storage, err := NewStorage(config)
				Expect(err).NotTo(HaveOccurred())
				Expect(storage).NotTo(BeNil())

				err = storage.Close()
				Expect(err).NotTo(HaveOccurred())
			})
		})

		Context("with HTTP storage type", func() {
			It("should create HTTP storage", func() {
				config := StorageConfig{
					Type:     "http",
					Endpoint: "http://storage.example.com",
				}

				storage, err := NewStorage(config)
				Expect(err).NotTo(HaveOccurred())
				Expect(storage).NotTo(BeNil())

				err = storage.Close()
				Expect(err).NotTo(HaveOccurred())
			})
		})

		Context("with HTTPS storage type", func() {
			It("should create HTTPS storage", func() {
				config := StorageConfig{
					Type:     "https",
					Endpoint: "https://storage.example.com",
				}

				storage, err := NewStorage(config)
				Expect(err).NotTo(HaveOccurred())
				Expect(storage).NotTo(BeNil())

				err = storage.Close()
				Expect(err).NotTo(HaveOccurred())
			})
		})

		Context("with unsupported storage type", func() {
			It("should return error", func() {
				config := StorageConfig{
					Type: "unsupported",
				}

				_, err := NewStorage(config)
				Expect(err).To(HaveOccurred())
				Expect(err).To(BeAssignableToTypeOf(&StorageError{}))

				storageErr, ok := err.(*StorageError)
				Expect(ok).To(BeTrue())
				Expect(storageErr.Type).To(Equal(ErrorTypeInvalidConfig))
			})
		})
	})

	Describe("StorageError", func() {
		It("should implement error interface", func() {
			err := &StorageError{
				Type:    ErrorTypeNotFound,
				Message: "file not found",
			}

			Expect(err.Error()).To(Equal("file not found"))
		})

		It("should include cause in error message", func() {
			cause := &StorageError{
				Type:    ErrorTypeNetworkError,
				Message: "connection timeout",
			}

			err := &StorageError{
				Type:    ErrorTypeOperationFailed,
				Message: "upload failed",
				Cause:   cause,
			}

			Expect(err.Error()).To(ContainSubstring("upload failed"))
			Expect(err.Error()).To(ContainSubstring("connection timeout"))
		})
	})
})
