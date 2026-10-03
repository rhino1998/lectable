package store

// rowhash and bit_xor are written in C and registered on every connection
// through sqlite3_auto_extension, rather than as Go callbacks: the
// fingerprints call them once per row of a whole book, and a Go callback
// costs several microseconds a call. The sqlite3 symbols come from
// go-sqlite3's bundled library, linked into the same binary.

/*
#cgo linux LDFLAGS: -Wl,--unresolved-symbols=ignore-in-object-files
#cgo darwin LDFLAGS: -Wl,-undefined,dynamic_lookup
#include <stdint.h>
#include <string.h>

typedef struct sqlite3 sqlite3;
typedef struct sqlite3_context sqlite3_context;
typedef struct sqlite3_value sqlite3_value;
typedef long long sqlite3_int64;

extern int sqlite3_auto_extension(void (*)(void));
extern int sqlite3_create_function(sqlite3 *, const char *, int, int, void *,
	void (*)(sqlite3_context *, int, sqlite3_value **),
	void (*)(sqlite3_context *, int, sqlite3_value **),
	void (*)(sqlite3_context *));
extern int sqlite3_value_type(sqlite3_value *);
extern const void *sqlite3_value_blob(sqlite3_value *);
extern int sqlite3_value_bytes(sqlite3_value *);
extern sqlite3_int64 sqlite3_value_int64(sqlite3_value *);
extern double sqlite3_value_double(sqlite3_value *);
extern void sqlite3_result_int64(sqlite3_context *, sqlite3_int64);
extern void *sqlite3_aggregate_context(sqlite3_context *, int);

#define LECTABLE_UTF8 1
#define LECTABLE_DETERMINISTIC 0x800
#define LECTABLE_INTEGER 1
#define LECTABLE_FLOAT 2
#define LECTABLE_NULL 5

static uint64_t fnv(uint64_t h, const unsigned char *p, size_t n) {
	for (size_t i = 0; i < n; i++) {
		h ^= p[i];
		h *= 1099511628211ULL;
	}
	return h;
}

// rowhash(v, ...): FNV-1a over the arguments in order, each tagged with
// its type and length so adjacent values can't run together.
static void rowhashFunc(sqlite3_context *ctx, int argc, sqlite3_value **argv) {
	uint64_t h = 14695981039346656037ULL;
	for (int i = 0; i < argc; i++) {
		unsigned char tag = (unsigned char)sqlite3_value_type(argv[i]);
		h = fnv(h, &tag, 1);
		if (tag == LECTABLE_NULL) {
			continue;
		}
		if (tag == LECTABLE_INTEGER) {
			sqlite3_int64 v = sqlite3_value_int64(argv[i]);
			h = fnv(h, (const unsigned char *)&v, sizeof v);
		} else if (tag == LECTABLE_FLOAT) {
			double v = sqlite3_value_double(argv[i]);
			h = fnv(h, (const unsigned char *)&v, sizeof v);
		} else {
			const unsigned char *p = sqlite3_value_blob(argv[i]);
			int64_t n = sqlite3_value_bytes(argv[i]);
			h = fnv(h, (const unsigned char *)&n, sizeof n);
			h = fnv(h, p, (size_t)n);
		}
	}
	sqlite3_result_int64(ctx, (sqlite3_int64)h);
}

static void xorStep(sqlite3_context *ctx, int argc, sqlite3_value **argv) {
	sqlite3_int64 *acc = sqlite3_aggregate_context(ctx, sizeof *acc);
	if (acc && sqlite3_value_type(argv[0]) != LECTABLE_NULL) {
		*acc ^= sqlite3_value_int64(argv[0]);
	}
}

static void xorFinal(sqlite3_context *ctx) {
	sqlite3_int64 *acc = sqlite3_aggregate_context(ctx, 0);
	sqlite3_result_int64(ctx, acc ? *acc : 0);
}

static int lectableInit(sqlite3 *db, char **err, const void *api) {
	int rc = sqlite3_create_function(db, "rowhash", -1, LECTABLE_UTF8 | LECTABLE_DETERMINISTIC, 0, rowhashFunc, 0, 0);
	if (rc != 0) {
		return rc;
	}
	return sqlite3_create_function(db, "bit_xor", 1, LECTABLE_UTF8 | LECTABLE_DETERMINISTIC, 0, 0, xorStep, xorFinal);
}

static int lectableRegister(void) {
	return sqlite3_auto_extension((void (*)(void))lectableInit);
}
*/
import "C"

import "fmt"

func init() {
	if rc := C.lectableRegister(); rc != 0 {
		panic(fmt.Sprintf("store: register SQLite functions: code %d", int(rc)))
	}
}
