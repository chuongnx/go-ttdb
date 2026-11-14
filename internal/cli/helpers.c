#include "cli.h"

SQLRETURN gtt_set_env_int(SQLHENV h, SQLINTEGER attr, SQLLEN v) {
	return SQLSetEnvAttr(h, attr, (SQLPOINTER)v, 0);
}

SQLRETURN gtt_set_conn_int(SQLHDBC h, SQLINTEGER attr, SQLLEN v) {
	return SQLSetConnectAttr(h, attr, (SQLPOINTER)v, 0);
}

SQLRETURN gtt_set_stmt_int(SQLHSTMT h, SQLINTEGER attr, SQLLEN v) {
	return SQLSetStmtAttr(h, attr, (SQLPOINTER)v, 0);
}

SQLRETURN gtt_set_stmt_ptr(SQLHSTMT h, SQLINTEGER attr, void *p) {
	return SQLSetStmtAttr(h, attr, (SQLPOINTER)p, 0);
}

SQLRETURN gtt_get_conn_uint(SQLHDBC h, SQLINTEGER attr, SQLUINTEGER *out) {
	return SQLGetConnectAttr(h, attr, (SQLPOINTER)out, 0, NULL);
}

int gtt_ok(SQLRETURN rc) {
	return rc == SQL_SUCCESS || rc == SQL_SUCCESS_WITH_INFO;
}
