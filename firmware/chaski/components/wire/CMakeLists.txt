# Dual-mode build (chaski-implementation-plan ground rule 3): the same sources
# build as an ESP-IDF component on the target and as a plain static library on
# the host, which is what lets the logic tiers be tested with no hardware.
if(ESP_PLATFORM)
  # `json` is IDF's cJSON component: the same <cjson/cJSON.h> the host tier
  # links from libcjson, so wire.cpp is byte-identical on both sides.
  idf_component_register(SRCS "wire.cpp"
                         INCLUDE_DIRS "include"
                         REQUIRES json)
else()
  add_library(chaski_wire STATIC wire.cpp)
  target_include_directories(chaski_wire PUBLIC include)
  target_compile_features(chaski_wire PUBLIC cxx_std_17)
  if(NOT "cjson" STREQUAL "")
    target_link_libraries(chaski_wire PUBLIC cjson)
  endif()
endif()
