# Dual-mode build (chaski-implementation-plan ground rule 3): the same sources
# build as an ESP-IDF component on the target and as a plain static library on
# the host, which is what lets the logic tiers be tested with no hardware.
#
# hmac_sha256.cpp is compiled on BOTH sides on purpose — see its header. The
# MAC the device checks must be the MAC the host tier proved, so mbedTLS is
# deliberately not a dependency here and REQUIRES stays empty.
if(ESP_PLATFORM)
  idf_component_register(SRCS "pututu.cpp" "hmac_sha256.cpp"
                         INCLUDE_DIRS "include"
                         REQUIRES )
else()
  add_library(chaski_pututu STATIC pututu.cpp hmac_sha256.cpp)
  target_include_directories(chaski_pututu PUBLIC include)
  target_compile_features(chaski_pututu PUBLIC cxx_std_17)
endif()
