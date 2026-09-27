// sample-c: a tiny server-rendered C app that follows the dootd app contract.
//
//   GET /          count a visit in SQLite and show the total
//   GET /healthz   200 "ok" (dootd health check)
//   GET /alloc?mb=N  allocate and keep N MB (to test the memory limit / OOM)
//   GET /crash     exit(1) (to test restarts and the crashed state)
//
// Contract: listen on $HOST:$PORT, SQLite in $DATA_DIR, logs to stdout/stderr,
// finish and exit on SIGTERM.
#include <arpa/inet.h>
#include <errno.h>
#include <netinet/in.h>
#include <poll.h>
#include <signal.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sys/socket.h>
#include <unistd.h>

#include "sqlite3.h"

static volatile sig_atomic_t stopping = 0;
static void on_term(int sig) { (void)sig; stopping = 1; }

static sqlite3 *db;

// Keeps /alloc memory reachable so the compiler cannot optimize it away.
static char *volatile leaked[1024];
static size_t nleaked;

static const char *env_or_die(const char *name) {
    const char *v = getenv(name);
    if (!v || !*v) {
        fprintf(stderr, "missing required env var %s\n", name);
        exit(2);
    }
    return v;
}

static void exec_or_die(const char *sql) {
    char *err = NULL;
    if (sqlite3_exec(db, sql, NULL, NULL, &err) != SQLITE_OK) {
        fprintf(stderr, "sqlite: %s: %s\n", sql, err);
        exit(1);
    }
}

static void open_db(const char *data_dir) {
    char path[4096];
    snprintf(path, sizeof path, "%s/app.db", data_dir);
    if (sqlite3_open(path, &db) != SQLITE_OK) {
        fprintf(stderr, "sqlite open %s: %s\n", path, sqlite3_errmsg(db));
        exit(1);
    }
    // Recommended settings from docs/app-contract.md §6.
    exec_or_die("PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000;"
                "PRAGMA foreign_keys=ON; PRAGMA synchronous=NORMAL;");
    // Migrations run at startup, before listening.
    exec_or_die("CREATE TABLE IF NOT EXISTS visits (id INTEGER PRIMARY KEY, at INTEGER NOT NULL)");
}

static long long record_visit(void) {
    exec_or_die("INSERT INTO visits (at) VALUES (unixepoch())");
    sqlite3_stmt *st;
    long long n = -1;
    if (sqlite3_prepare_v2(db, "SELECT count(*) FROM visits", -1, &st, NULL) == SQLITE_OK) {
        if (sqlite3_step(st) == SQLITE_ROW) n = sqlite3_column_int64(st, 0);
        sqlite3_finalize(st);
    }
    return n;
}

static void respond(int fd, int status, const char *reason, const char *ctype, const char *body) {
    char head[256];
    int bl = (int)strlen(body);
    int hl = snprintf(head, sizeof head,
                      "HTTP/1.1 %d %s\r\nContent-Type: %s\r\nContent-Length: %d\r\n"
                      "Connection: close\r\n\r\n",
                      status, reason, ctype, bl);
    if (write(fd, head, (size_t)hl) < 0 || write(fd, body, (size_t)bl) < 0) {
        /* client went away */
    }
}

static void handle(int fd) {
    char req[8192];
    ssize_t n = read(fd, req, sizeof req - 1);
    if (n <= 0) return;
    req[n] = 0;

    char method[8] = {0}, target[2048] = {0};
    if (sscanf(req, "%7s %2047s", method, target) != 2) {
        respond(fd, 400, "Bad Request", "text/plain", "bad request\n");
        return;
    }
    printf("%s %s\n", method, target);

    if (strcmp(method, "GET") != 0) {
        respond(fd, 405, "Method Not Allowed", "text/plain", "method not allowed\n");
    } else if (strcmp(target, "/healthz") == 0) {
        respond(fd, 200, "OK", "text/plain", "ok\n");
    } else if (strcmp(target, "/") == 0) {
        char body[512];
        snprintf(body, sizeof body,
                 "<!doctype html><title>sample-c</title>"
                 "<h1>Hello from C on dootd</h1><p>Visits: %lld</p><p>Release: %s</p>\n",
                 record_visit(), getenv("DOOTD_RELEASE") ? getenv("DOOTD_RELEASE") : "?");
        respond(fd, 200, "OK", "text/html; charset=utf-8", body);
    } else if (strncmp(target, "/alloc?mb=", 10) == 0) {
        long mb = strtol(target + 10, NULL, 10);
        if (mb <= 0 || mb > 65536) {
            respond(fd, 400, "Bad Request", "text/plain", "mb must be 1..65536\n");
            return;
        }
        char *p = malloc((size_t)mb << 20);
        if (!p) {
            respond(fd, 500, "Internal Server Error", "text/plain", "malloc failed\n");
            return;
        }
        // Fill with pseudo-random bytes so the pages really count (and can't
        // be compressed away by zswap). Intentionally leaked.
        uint64_t x = 88172645463325252ull;
        for (size_t i = 0; i < ((size_t)mb << 20); i += 8) {
            x ^= x << 13; x ^= x >> 7; x ^= x << 17;
            memcpy(p + i, &x, 8);
        }
        leaked[nleaked++ % 1024] = p;
        respond(fd, 200, "OK", "text/plain", "allocated\n");
    } else if (strcmp(target, "/crash") == 0) {
        respond(fd, 200, "OK", "text/plain", "crashing\n");
        fprintf(stderr, "crash requested\n");
        exit(1);
    } else {
        respond(fd, 404, "Not Found", "text/plain", "not found\n");
    }
}

int main(void) {
    // stdout is a pipe under dootd: line-buffer it so logs appear immediately.
    setvbuf(stdout, NULL, _IOLBF, 0);

    const char *host = env_or_die("HOST");
    int port = atoi(env_or_die("PORT"));
    open_db(env_or_die("DATA_DIR"));

    struct sigaction sa = {0};
    sa.sa_handler = on_term;
    sigaction(SIGTERM, &sa, NULL);
    sigaction(SIGINT, &sa, NULL);

    int ls = socket(AF_INET, SOCK_STREAM | SOCK_CLOEXEC, 0);
    int one = 1;
    setsockopt(ls, SOL_SOCKET, SO_REUSEADDR, &one, sizeof one);
    struct sockaddr_in addr = {.sin_family = AF_INET, .sin_port = htons((uint16_t)port)};
    if (inet_pton(AF_INET, host, &addr.sin_addr) != 1 ||
        bind(ls, (struct sockaddr *)&addr, sizeof addr) != 0 || listen(ls, 128) != 0) {
        fprintf(stderr, "listen %s:%d: %s\n", host, port, strerror(errno));
        return 1;
    }
    printf("listening on %s:%d\n", host, port);

    while (!stopping) {
        struct pollfd pfd = {.fd = ls, .events = POLLIN};
        int r = poll(&pfd, 1, 500);  // wake up regularly to notice SIGTERM
        if (r <= 0) continue;
        int c = accept4(ls, NULL, NULL, SOCK_CLOEXEC);
        if (c < 0) continue;
        handle(c);
        close(c);
    }

    printf("SIGTERM received, shutting down\n");
    close(ls);
    sqlite3_close(db);
    return 0;
}
