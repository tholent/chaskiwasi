# Chaski firmware — ESP-IDF project (target build).
#
# Host tests do NOT use this file: they build the same components through
# test/firmware/host/CMakeLists.txt with no IDF installed. That split is the
# point of implementation-plan ground rule 3, and it is what keeps waves 1-2
# moving without hardware.
cmake_minimum_required(VERSION 3.16)

set(EXTRA_COMPONENT_DIRS "${CMAKE_CURRENT_LIST_DIR}/components")

include($ENV{IDF_PATH}/tools/cmake/project.cmake)
project(chaski)
