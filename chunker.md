# Chunker

Let's say we have a directory struture like that:

- dir1 26.7mb
  - dir11 20.1mb
    - file111 0.1 mb
    - file112 0.1 mb
    - dir111 20 mb
      - file1111 0.1mb
      - file1112 0.1mb
      - file1113 0.1mb
      - ...
  - dir12 6.6mb
    - file121 0.3 mb
    - file122 0.2 mb
    - dir121 5.6mb
      - file1211 0.5mb
      - file1212 0.5mb
      - dir1211 4.2mb
        - dir12111 1.1mb
          - file 121111 0.1mb
          - file 121112 0.1mb
          - file 121113 0.1mb
          - file 121114 0.1mb
          - ...
        - dir12112 1.1mb
          - ...
        - dir12113 1.1mb
          - ...
    - dir122 0.5mb
      - file1221 0.5mb

That means that:

- a single chunk for directory dir122.
- a single chunk for directory dir12111.
- a single chunk for directory dir12112
- a single chunk for directory dir12113.
- a single chunk for directory dir121 with only file1211 and file1212
- many chunk for dir111 because a big directory like that can be append all together
- a single chunk for dir11 with only file111 and file112
