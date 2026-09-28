//! sample-zig: a tiny server-rendered Zig app that follows the dootd app contract.
//!
//!   GET /            count a visit in SQLite and show the total
//!   GET /healthz     200 "ok" (dootd health check)
//!   GET /alloc?mb=N  allocate and keep N MB (to test the memory limit / OOM)
//!   GET /crash       exit(1) (to test restarts and the crashed state)
//!
//! Written for Zig 0.16. Networking and signals use libc (std.c) so the code
//! stays small and does not depend on the fast-moving std.Io APIs.
const std = @import("std");
const c = @import("c"); // sqlite3.h via translate-c (see build.zig)
const libc = std.c;

var stopping = std.atomic.Value(bool).init(false);

fn onTerm(_: libc.SIG) callconv(.c) void {
    stopping.store(true, .seq_cst);
}

var db: ?*c.sqlite3 = null;

fn log(comptime fmt: []const u8, args: anytype) void {
    std.debug.print(fmt ++ "\n", args); // stderr, unbuffered
}

fn envOrDie(env: *std.process.Environ.Map, name: []const u8) []const u8 {
    const v = env.get(name) orelse {
        log("missing required env var {s}", .{name});
        std.process.exit(2);
    };
    if (v.len == 0) {
        log("missing required env var {s}", .{name});
        std.process.exit(2);
    }
    return v;
}

fn execOrDie(sql: [*:0]const u8) void {
    var err: [*c]u8 = null;
    if (c.sqlite3_exec(db, sql, null, null, &err) != c.SQLITE_OK) {
        log("sqlite: {s}: {s}", .{ sql, err });
        std.process.exit(1);
    }
}

fn openDb(data_dir: []const u8) void {
    var buf: [4096]u8 = undefined;
    const path = std.fmt.bufPrintZ(&buf, "{s}/app.db", .{data_dir}) catch {
        log("DATA_DIR too long", .{});
        std.process.exit(1);
    };
    if (c.sqlite3_open(path.ptr, &db) != c.SQLITE_OK) {
        log("sqlite open {s}: {s}", .{ path, c.sqlite3_errmsg(db) });
        std.process.exit(1);
    }
    // Recommended settings from docs/app-contract.md §6.
    execOrDie("PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000; PRAGMA foreign_keys=ON; PRAGMA synchronous=NORMAL;");
    // Migrations run at startup, before listening.
    execOrDie("CREATE TABLE IF NOT EXISTS visits (id INTEGER PRIMARY KEY, at INTEGER NOT NULL)");
}

fn recordVisit() i64 {
    execOrDie("INSERT INTO visits (at) VALUES (unixepoch())");
    var st: ?*c.sqlite3_stmt = null;
    if (c.sqlite3_prepare_v2(db, "SELECT count(*) FROM visits", -1, &st, null) != c.SQLITE_OK) return -1;
    defer _ = c.sqlite3_finalize(st);
    if (c.sqlite3_step(st) != c.SQLITE_ROW) return -1;
    return c.sqlite3_column_int64(st, 0);
}

fn writeAll(fd: libc.fd_t, data: []const u8) void {
    var off: usize = 0;
    while (off < data.len) {
        const n = libc.write(fd, data[off..].ptr, data.len - off);
        if (n <= 0) return; // client went away
        off += @intCast(n);
    }
}

fn respond(fd: libc.fd_t, status: []const u8, ctype: []const u8, body: []const u8) void {
    var head: [256]u8 = undefined;
    const h = std.fmt.bufPrint(&head, "HTTP/1.1 {s}\r\nContent-Type: {s}\r\nContent-Length: {d}\r\nConnection: close\r\n\r\n", .{ status, ctype, body.len }) catch return;
    writeAll(fd, h);
    writeAll(fd, body);
}

fn handle(fd: libc.fd_t, release: []const u8) void {
    var req: [8192]u8 = undefined;
    const n = libc.read(fd, &req, req.len);
    if (n <= 0) return;
    const line_end = std.mem.indexOfScalar(u8, req[0..@intCast(n)], '\r') orelse @as(usize, @intCast(n));
    var it = std.mem.tokenizeScalar(u8, req[0..line_end], ' ');
    const method = it.next() orelse return respond(fd, "400 Bad Request", "text/plain", "bad request\n");
    const target = it.next() orelse return respond(fd, "400 Bad Request", "text/plain", "bad request\n");
    log("{s} {s}", .{ method, target });

    if (!std.mem.eql(u8, method, "GET")) {
        respond(fd, "405 Method Not Allowed", "text/plain", "method not allowed\n");
    } else if (std.mem.eql(u8, target, "/healthz")) {
        respond(fd, "200 OK", "text/plain", "ok\n");
    } else if (std.mem.eql(u8, target, "/")) {
        var body: [512]u8 = undefined;
        const b = std.fmt.bufPrint(&body, "<!doctype html><title>sample-zig</title><h1>Hello from Zig on dootd</h1><p>Visits: {d}</p><p>Release: {s}</p>\n", .{ recordVisit(), release }) catch return;
        respond(fd, "200 OK", "text/html; charset=utf-8", b);
    } else if (std.mem.startsWith(u8, target, "/alloc?mb=")) {
        const mb = std.fmt.parseInt(usize, target["/alloc?mb=".len..], 10) catch 0;
        if (mb == 0 or mb > 65536) return respond(fd, "400 Bad Request", "text/plain", "mb must be 1..65536\n");
        const p: ?[*]u8 = @ptrCast(libc.malloc(mb << 20));
        const mem = p orelse return respond(fd, "500 Internal Server Error", "text/plain", "malloc failed\n");
        // Fill with pseudo-random bytes so the pages really count (and can't
        // be compressed away by zswap). Intentionally leaked.
        var prng = std.Random.DefaultPrng.init(0x5eed);
        prng.random().bytes(mem[0 .. mb << 20]);
        std.mem.doNotOptimizeAway(mem);
        respond(fd, "200 OK", "text/plain", "allocated\n");
    } else if (std.mem.eql(u8, target, "/crash")) {
        respond(fd, "200 OK", "text/plain", "crashing\n");
        log("crash requested", .{});
        std.process.exit(1);
    } else {
        respond(fd, "404 Not Found", "text/plain", "not found\n");
    }
}

pub fn main(init: std.process.Init) !void {
    const env = init.environ_map;
    const host = envOrDie(env, "HOST");
    const port = std.fmt.parseInt(u16, envOrDie(env, "PORT"), 10) catch {
        log("PORT must be a number", .{});
        std.process.exit(2);
    };
    openDb(envOrDie(env, "DATA_DIR"));
    const release = env.get("DOOTD_RELEASE") orelse "?";

    var sa = std.mem.zeroes(libc.Sigaction);
    sa.handler = .{ .handler = onTerm };
    _ = libc.sigaction(.TERM, &sa, null);
    _ = libc.sigaction(.INT, &sa, null);

    const ip = std.Io.net.Ip4Address.parse(host, port) catch {
        log("HOST must be an IPv4 address, got {s}", .{host});
        std.process.exit(2);
    };
    const ls = libc.socket(libc.AF.INET, libc.SOCK.STREAM | libc.SOCK.CLOEXEC, 0);
    const one: c_int = 1;
    _ = libc.setsockopt(ls, libc.SOL.SOCKET, libc.SO.REUSEADDR, &one, @sizeOf(c_int));
    var addr: libc.sockaddr.in = .{
        .port = std.mem.nativeToBig(u16, port),
        .addr = @bitCast(ip.bytes),
    };
    if (libc.bind(ls, @ptrCast(&addr), @sizeOf(libc.sockaddr.in)) != 0 or libc.listen(ls, 128) != 0) {
        log("listen {s}:{d} failed (errno {d})", .{ host, port, @intFromEnum(libc.errno(-1)) });
        std.process.exit(1);
    }
    log("listening on {s}:{d}", .{ host, port });

    while (!stopping.load(.seq_cst)) {
        var pfd = [_]libc.pollfd{.{ .fd = ls, .events = libc.POLL.IN, .revents = 0 }};
        if (libc.poll(&pfd, 1, 500) <= 0) continue; // wake up regularly to notice SIGTERM
        const conn = libc.accept4(ls, null, null, libc.SOCK.CLOEXEC);
        if (conn < 0) continue;
        handle(conn, release);
        _ = libc.close(conn);
    }

    log("SIGTERM received, shutting down", .{});
    _ = libc.close(ls);
    _ = c.sqlite3_close(db);
}
