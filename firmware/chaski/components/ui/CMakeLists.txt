# Dual-mode build (chaski-implementation-plan ground rule 3): the same sources
# build as an ESP-IDF component on the target and as a plain static library on
# the host, which is what lets the logic tiers be tested with no hardware.
if(ESP_PLATFORM)
  idf_component_register(SRCS "ui.cpp"
                         INCLUDE_DIRS "include"
                         REQUIRES wire store ayllu layout panel input)
else()
  add_library(chaski_ui STATIC ui.cpp)
  target_include_directories(chaski_ui PUBLIC include)
  target_compile_features(chaski_ui PUBLIC cxx_std_17)
  if(NOT "chaski_wire chaski_store chaski_ayllu chaski_layout chaski_panel chaski_input" STREQUAL "")
    target_link_libraries(chaski_ui PUBLIC chaski_wire chaski_store chaski_ayllu chaski_layout chaski_panel chaski_input)
  endif()
endif()
