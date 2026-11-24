-- 0. 確認是新容器（伺服器 collation 應為 Chinese_Taiwan_Stroke_CI_AS）
SELECT @@SERVERNAME AS server_name,
       SERVERPROPERTY('Collation') AS server_collation,
       DATABASEPROPERTYEX('hrm_0907', 'Collation') AS db_collation;


GO
-- 1. 登入（密碼沿用 ~/.config/hrm-sql-mcp/credentials.env 的 HRM_SQL_LOCAL_HRM_RO_PASSWORD）
USE master;


GO
CREATE LOGIN hrm_mcp_ro
    WITH PASSWORD = N'YouCantSeeMe', DEFAULT_DATABASE = [hrm_0907], CHECK_POLICY = ON, CHECK_EXPIRATION = OFF;


GO
-- 2. 資料庫授權
USE [hrm_0907];


GO
CREATE USER hrm_mcp_ro FOR LOGIN hrm_mcp_ro;

ALTER ROLE db_datareader ADD MEMBER hrm_mcp_ro;

GRANT VIEW DEFINITION TO hrm_mcp_ro; -- 漏掉的話，sql_modules.definition 會靜悄悄地回傳 NULL

GRANT SHOWPLAN TO hrm_mcp_ro;

-- 護欄：DENY 永遠贏過 GRANT
DENY EXECUTE TO hrm_mcp_ro;

DENY INSERT, UPDATE, DELETE TO hrm_mcp_ro;

DENY ALTER, ALTER ANY SCHEMA, CREATE TABLE, CREATE PROCEDURE, CREATE VIEW, CREATE FUNCTION, BACKUP DATABASE, BACKUP LOG TO hrm_mcp_ro;


GO
-- ⚠ 絕對不要 DENY CONTROL：會連帶擋掉 CONNECT，登入時出現 Msg 4060
-- 3. 檢查權限清單：VIEW DEFINITION／SHOWPLAN／CONNECT 應為 GRANT，其餘應為 DENY
SELECT   permission_name,
         state_desc
FROM     sys.database_permissions
WHERE    grantee_principal_id = DATABASE_PRINCIPAL_ID('hrm_mcp_ro')
ORDER BY state_desc, permission_name;