# Dual-mode build (chaski-implementation-plan ground rule 3): the same sources
# build as an ESP-IDF component on the target and as a plain static library on
# the host, which is what lets the logic tiers be tested with no hardware.
if(ESP_PLATFORM)
  idf_component_register(SRCS "layout.cpp"
                         INCLUDE_DIRS "include"
                         REQUIRES )
else()
  add_library(chaski_layout STATIC layout.cpp)
  target_include_directories(chaski_layout PUBLIC include)
  target_compile_features(chaski_layout PUBLIC cxx_std_17)
  if(NOT "utf8proc" STREQUAL "")
    target_link_libraries(chaski_layout PUBLIC utf8proc)
  endif()
endif()
