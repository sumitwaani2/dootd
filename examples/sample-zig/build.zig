const std = @import("std");

pub fn build(b: *std.Build) void {
    const target = b.standardTargetOptions(.{});
    const optimize = b.standardOptimizeOption(.{});

    const sqlite = b.dependency("sqlite", .{});

    // sqlite3.h -> Zig bindings, imported in main.zig as @import("c").
    const c = b.addTranslateC(.{
        .root_source_file = sqlite.path("sqlite3.h"),
        .target = target,
        .optimize = optimize,
    });

    const mod = b.createModule(.{
        .root_source_file = b.path("src/main.zig"),
        .target = target,
        .optimize = optimize,
        .link_libc = true,
        .imports = &.{.{ .name = "c", .module = c.createModule() }},
    });
    mod.addCSourceFile(.{
        .file = sqlite.path("sqlite3.c"),
        .flags = &.{ "-DSQLITE_THREADSAFE=0", "-DSQLITE_DQS=0", "-DSQLITE_OMIT_LOAD_EXTENSION" },
    });

    const exe = b.addExecutable(.{ .name = "sample-zig", .root_module = mod });
    b.installArtifact(exe);

    // `zig build test`: run by the release workflow before building.
    const tests = b.addTest(.{ .root_module = mod });
    b.step("test", "Run the tests").dependOn(&b.addRunArtifact(tests).step);
}
