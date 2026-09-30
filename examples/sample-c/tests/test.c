// `make test`: run by the release workflow before building. It includes the
// app itself (with its main renamed) to test the SQLite code directly.
#define main sample_main
#include "../src/main.c"
#undef main

static int fails;
#define CHECK(cond) do { if (!(cond)) { fprintf(stderr, "FAIL %s:%d: %s\n", __FILE__, __LINE__, #cond); fails++; } } while (0)

int main(void) {
    char dir[] = "/tmp/sample-c-test-XXXXXX";
    CHECK(mkdtemp(dir) != NULL);

    open_db(dir);
    CHECK(record_visit() == 1);
    CHECK(record_visit() == 2);
    sqlite3_close(db);

    // Visits survive a restart (the database lives in DATA_DIR).
    open_db(dir);
    CHECK(record_visit() == 3);
    sqlite3_close(db);

    if (fails) return 1;
    printf("ok: sample-c tests passed\n");
    return 0;
}
