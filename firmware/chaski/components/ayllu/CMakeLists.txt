# Dual-mode build (chaski-implementation-plan ground rule 3): the same sources
# build as an ESP-IDF component on the target and as a plain static library on
# the host, which is what lets the logic tiers be tested with no hardware.
#
# The dependency on `store` is for chaski/fsutil.h alone: the snapshot and the
# overlay need the same atomic-replacement discipline as the letter store, and
# one implementation of that discipline is worth more than two (client §4).
if(ESP_PLATFORM)
  idf_component_register(SRCS "ayllu.cpp"
                         INCLUDE_DIRS "include"
                         REQUIRES wire store)
else()
  add_library(chaski_ayllu STATIC ayllu.cpp)
  target_include_directories(chaski_ayllu PUBLIC include)
  target_compile_features(chaski_ayllu PUBLIC cxx_std_17)
  if(NOT "chaski_wire" STREQUAL "")
    target_link_libraries(chaski_ayllu PUBLIC chaski_wire chaski_store)
  endif()
endif()
