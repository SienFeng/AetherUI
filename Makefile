# AetherUI 构建与验证入口。
#
# CGO 必须开启：gorm.io/driver/sqlite 依赖 mattn/go-sqlite3，
# CGO_ENABLED=0 的构建会在运行时打不开数据库。
export CGO_ENABLED := 1

BINARY := a-ui

.PHONY: help build test vet verify clean

help:
	@echo "make build   编译 $(BINARY)"
	@echo "make test    跑全部 Go 测试"
	@echo "make vet     go vet ./..."
	@echo "make verify  vet + test + build，提交前的门禁"
	@echo "make clean   删除构建产物"

build:
	go build -trimpath -ldflags "-s -w" -o $(BINARY) main.go

# web/service 的 TestMain 会 chdir 到仓库根，需要 bin/xray-$(GOOS)-$(GOARCH) 在位。
test:
	go test ./...

vet:
	go vet ./...

verify: vet test build

clean:
	rm -f $(BINARY)
