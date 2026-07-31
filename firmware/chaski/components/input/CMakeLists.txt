# Dual-mode build (chaski-implementation-plan ground rule 3): the same sources
# build as an ESP-IDF component on the target and as a plain static library on
# the host, which is what lets the logic tiers be tested with no hardware.
if(ESP_PLATFORM)
  idf_component_register(SRCS "input.cpp"
                         INCLUDE_DIRS "include"
                         REQUIRES )
else()
  add_library(chaski_input STATIC input.cpp)
  target_include_directories(chaski_input PUBLIC include)
  target_compile_features(chaski_input PUBLIC cxx_std_17)
  if(NOT "" STREQUAL "")
    target_link_libraries(chaski_input PUBLIC )
  endif()
endif()
