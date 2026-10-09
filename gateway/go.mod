module nextemby-replay/gateway

go 1.24.6

require (
	nextemby-replay/adapter115 v0.0.0-00010101000000-000000000000
	nextemby-replay/engine v0.0.0
	nextemby-replay/store v0.0.0-00010101000000-000000000000
)

require (
	github.com/SheltonZhu/115driver v1.3.5 // indirect
	github.com/aead/ecdh v0.2.0 // indirect
	github.com/aliyun/aliyun-oss-go-sdk v3.0.2+incompatible // indirect
	github.com/andreburgaud/crypt2go v1.1.0 // indirect
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/go-resty/resty/v2 v2.17.2 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/kr/text v0.2.0 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/pierrec/lz4/v4 v4.1.17 // indirect
	github.com/pkg/errors v0.9.1 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	github.com/rogpeppe/go-internal v1.9.0 // indirect
	github.com/skip2/go-qrcode v0.0.0-20200617195104-da1b6568686e // indirect
	golang.org/x/crypto v0.41.0 // indirect
	golang.org/x/net v0.43.0 // indirect
	golang.org/x/sys v0.35.0 // indirect
	golang.org/x/time v0.12.0 // indirect
	modernc.org/libc v1.55.3 // indirect
	modernc.org/mathutil v1.6.0 // indirect
	modernc.org/memory v1.8.0 // indirect
	modernc.org/sqlite v1.34.5 // indirect
)

replace nextemby-replay/engine => ../engine

replace nextemby-replay/adapter115 => ../adapter115

replace nextemby-replay/store => ../store
