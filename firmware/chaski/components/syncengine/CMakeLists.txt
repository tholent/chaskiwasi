# Dual-mode build (chaski-implementation-plan ground rule 3): the same sources
# build as an ESP-IDF component on the target and as a plain static library on
# the host, which is what lets the logic tiers be tested with no hardware.
if(ESP_PLATFORM)
  idf_component_register(SRCS "syncengine.cpp"
                         INCLUDE_DIRS "include"
                         REQUIRES wire store ayllu transport)
else()
  add_library(chaski_syncengine STATIC syncengine.cpp)
  target_include_directories(chaski_syncengine PUBLIC include)
  target_compile_features(chaski_syncengine PUBLIC cxx_std_17)
  if(NOT "chaski_wire chaski_store chaski_ayllu chaski_transport" STREQUAL "")
    target_link_libraries(chaski_syncengine PUBLIC chaski_wire chaski_store chaski_ayllu chaski_transport)
  endif()
endif()
