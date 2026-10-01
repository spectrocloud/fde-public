
1. Extract the content bundle ZST to disk:
   ```
   tar --zstd -xvf <your content bundle>.tar.zst
   ```

2. Find the digest for the cluster config:
   ```
   cat index.json | jq '.manifests.[] | select(.annotations["spectrocloud.bundle.artifact.rawfile.filename"]=="output/combined/spectro-ec-appliance.tgz") .digest'

   returns:
   "sha256:360091ddc8c16ed2395d7e1d4c21e70f4f85933a6e5f44fa57202916a26f5da6",
   ```

3. Find the digest for the actual tarball from the digest file in the previous step:
   ```
   cat blobs/sha256/360091ddc8c16ed2395d7e1d4c21e70f4f85933a6e5f44fa57202916a26f5da6 | jq '.layers[0].digest'

   returns:
   "sha256:9bff8c64a3d7ff3bdf449d0b0933e39dd18d5f2cea27bcb4cedc49b79f938284"
   ```

4. Extract the tarball:
   ```
   gunzip -c blobs/sha256/9bff8c64a3d7ff3bdf449d0b0933e39dd18d5f2cea27bcb4cedc49b79f938284 | tar -xvf -
   ```

5. You can now find the `spc.json` in spc/apps