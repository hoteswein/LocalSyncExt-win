module localsyncext

go 1.22

require (
	github.com/kr/fs v0.1.0
	github.com/pkg/sftp v1.13.6
	golang.org/x/crypto v0.28.0
	golang.org/x/sys v0.26.0
)

replace (
	github.com/kr/fs => ./third_party/kr-fs
	github.com/pkg/sftp => ./third_party/sftp
	golang.org/x/crypto => ./third_party/crypto
	golang.org/x/sys => ./third_party/sys
)
