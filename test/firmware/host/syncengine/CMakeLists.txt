# The unit suite runs the engine on in-memory doubles; the integration suite
# runs the same engine on the real file-backed stores (implementation-plan §4).
chaski_add_test(syncengine_test syncengine_test.cpp chaski_syncengine)
chaski_add_test(syncengine_integration_test syncengine_integration_test.cpp chaski_syncengine)
