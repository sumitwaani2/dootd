# Built by .github/workflows/release.yml: make CC="zig cc -target x86_64-linux-musl"
# Locally: make CC="zig cc" && make test CC="zig cc"
CC     ?= cc
CFLAGS ?= -O2 -g0
CFLAGS += -std=c11 -Wall -Wextra -D_GNU_SOURCE -Ithird_party

SQLITE_VER    := 3530400
SQLITE_YEAR   := 2026
SQLITE_TGZ    := sqlite-autoconf-$(SQLITE_VER).tar.gz
SQLITE_SHA256 := 0e9483900e92cd5de8fd48d16bf9200145a61f7fd5be542a5ac81d8a9516eb9c
SQLITE_FLAGS  := -DSQLITE_THREADSAFE=0 -DSQLITE_DQS=0 -DSQLITE_OMIT_LOAD_EXTENSION -DSQLITE_DEFAULT_WAL_SYNCHRONOUS=1

build/sample-c: src/main.c build/sqlite3.o | build
	$(CC) $(CFLAGS) -o $@ src/main.c build/sqlite3.o -lm

build/sqlite3.o: third_party/sqlite3.c | build
	$(CC) -O2 -g0 $(SQLITE_FLAGS) -c -o $@ $<

# Fetch the SQLite amalgamation once and verify it.
third_party/sqlite3.c:
	mkdir -p third_party
	curl -fsSL -o third_party/$(SQLITE_TGZ) https://sqlite.org/$(SQLITE_YEAR)/$(SQLITE_TGZ)
	echo "$(SQLITE_SHA256)  third_party/$(SQLITE_TGZ)" | sha256sum -c -
	tar -xzf third_party/$(SQLITE_TGZ) -C third_party --strip-components=1 \
		sqlite-autoconf-$(SQLITE_VER)/sqlite3.c sqlite-autoconf-$(SQLITE_VER)/sqlite3.h
	rm third_party/$(SQLITE_TGZ)

build/test: tests/test.c src/main.c build/sqlite3.o | build
	$(CC) $(CFLAGS) -o $@ tests/test.c build/sqlite3.o -lm

test: build/test
	./build/test

build:
	mkdir -p build

clean:
	rm -rf build third_party

.PHONY: clean test
