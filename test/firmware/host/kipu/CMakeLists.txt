# cjson is needed directly: the block's serialised size is only decided by the
# wire encoder, so the suite measures it there rather than trusting an estimate.
chaski_add_test(kipu_test kipu_test.cpp chaski_kipu cjson)
