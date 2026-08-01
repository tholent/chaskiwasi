# Dual-mode build (chaski-implementation-plan ground rule 3): the same sources
# build as an ESP-IDF component on the target and as a plain static library on
# the host, which is what lets the logic tiers be tested with no hardware.
#
# usbcdc_link.cpp is the exception and the only file here that includes esp_*
# headers: it is the seam's hardware end, compiled on the target only. Host
# tests drive UsbBridgeTransport through an in-memory SerialLink instead.
if(ESP_PLATFORM)
  idf_component_register(SRCS "frame.cpp" "usbbridge.cpp" "usbcdc_link.cpp"
                         INCLUDE_DIRS "include"
                         REQUIRES esp_driver_usb_serial_jtag esp_timer freertos)
else()
  add_library(chaski_transport STATIC frame.cpp usbbridge.cpp)
  target_include_directories(chaski_transport PUBLIC include)
  target_compile_features(chaski_transport PUBLIC cxx_std_17)
endif()
