# cjson parses the committed token vectors; the private include path is what
# lets the suite check SHA-256 and HMAC against their published vectors as well
# as against the server's tokens.
chaski_add_test(pututu_test pututu_test.cpp chaski_pututu cjson)
target_include_directories(pututu_test PRIVATE ${CHASKI_COMPONENTS}/pututu)
