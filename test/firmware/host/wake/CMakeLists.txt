# The wake policy lives in main/ because it is the composition root's job
# (client §6), and main/ is not a component — so this target compiles the one
# portable translation unit there directly. wake.cpp includes no esp_* header
# for exactly this reason: the policy is what is worth testing, and no board
# exists to test it on (implementation-plan ground rule 3).
set(CHASKI_MAIN ${CMAKE_CURRENT_LIST_DIR}/../../../../firmware/chaski/main)

chaski_add_test(wake_test wake_test.cpp chaski_pututu)
target_sources(wake_test PRIVATE ${CHASKI_MAIN}/wake.cpp)
target_include_directories(wake_test PRIVATE ${CHASKI_MAIN})
