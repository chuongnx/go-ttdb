#ifndef GO_TIMESTEN_CLI_H
#define GO_TIMESTEN_CLI_H

#include <stdlib.h>
#include <string.h>
#include <timesten.h>
#include <sql.h>
#include <sqlext.h>

/* Attribute setters taking integer values (avoids integer→pointer conversions in Go). */
SQLRETURN gtt_set_env_int(SQLHENV h, SQLINTEGER attr, SQLLEN v);
SQLRETURN gtt_set_conn_int(SQLHDBC h, SQLINTEGER attr, SQLLEN v);
SQLRETURN gtt_set_stmt_int(SQLHSTMT h, SQLINTEGER attr, SQLLEN v);
SQLRETURN gtt_set_stmt_ptr(SQLHSTMT h, SQLINTEGER attr, void *p);
SQLRETURN gtt_get_conn_uint(SQLHDBC h, SQLINTEGER attr, SQLUINTEGER *out);
int gtt_ok(SQLRETURN rc);

#endif
